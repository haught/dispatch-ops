package api

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/rbac"
	"dispatch-ops/internal/store"
)

// syncCredentialKinds are the credential kinds an inventory sync can use:
// generic credentials become environment variables (what inventory plugins
// read), vault passwords decrypt vaulted inventory files, and every
// credential is in DISPATCH_CREDENTIALS for inventory scripts.
var syncCredentialKinds = map[string]bool{"generic": true, "vault": true, "api_token": true}

// checkNotSynced refuses manual changes to the hosts and groups of an
// inventory that is synced from a source.
func (h *Handlers) checkNotSynced(ctx context.Context, inventoryID uuid.UUID) error {
	src, err := h.Store.GetInventorySource(ctx, inventoryID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return conflict("this inventory is synced from %s in project %q, so its hosts and groups can't be edited by hand; "+
		"change the source, or remove it to edit them", src.SourcePath, src.ProjectName)
}

func toAPIInventorySource(s store.InventorySource) apigen.InventorySource {
	out := apigen.InventorySource{
		InventoryId: s.InventoryID, ProjectId: s.ProjectID, ProjectName: s.ProjectName, SourcePath: s.SourcePath,
		ExecutionEnvironmentId: s.ExecutionEnvironmentID, ExecutionEnvironmentName: s.ExecutionEnvironmentName,
		CredentialIds: nonNilSlice(s.CredentialIDs), TimeoutSeconds: s.TimeoutSeconds, Verbosity: s.Verbosity,
		LastJobId: s.LastJobID, LastSyncedAt: s.LastSyncedAt, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
	if s.LastJobStatus != nil {
		st := apigen.JobStatus(*s.LastJobStatus)
		out.LastJobStatus = &st
	}
	return out
}

func (h *Handlers) GetInventorySource(ctx context.Context, req apigen.GetInventorySourceRequestObject) (apigen.GetInventorySourceResponseObject, error) {
	if err := h.authorize(ctx, rbac.Inventory, req.InventoryId, rbac.Read); err != nil {
		return nil, err
	}
	src, err := h.Store.GetInventorySource(ctx, req.InventoryId)
	if err != nil {
		return nil, orNotFound(err, "Inventory source")
	}
	return apigen.GetInventorySource200JSONResponse(toAPIInventorySource(src)), nil
}

// checkSourcePath checks that an inventory file path stays inside the
// project.
func checkSourcePath(p string) error {
	if strings.TrimSpace(p) != p || !filepath.IsLocal(p) {
		return badRequest("sourcePath %q must be a relative path inside the project", p)
	}
	return nil
}

func (h *Handlers) SetInventorySource(ctx context.Context, req apigen.SetInventorySourceRequestObject) (apigen.SetInventorySourceResponseObject, error) {
	if err := h.authorize(ctx, rbac.Inventory, req.InventoryId, rbac.Admin); err != nil {
		return nil, err
	}
	inv, err := h.Store.GetInventory(ctx, req.InventoryId)
	if err != nil {
		return nil, orNotFound(err, "Inventory")
	}
	b := req.Body
	f := store.InventorySourceFields{
		ProjectID: b.ProjectId, SourcePath: b.SourcePath, ExecutionEnvironmentID: b.ExecutionEnvironmentId,
		CredentialIDs: dedupe(deref(b.CredentialIds)), TimeoutSeconds: 600,
	}
	setIf(&f.TimeoutSeconds, b.TimeoutSeconds)
	setIf(&f.Verbosity, b.Verbosity)
	if err := checkSourcePath(f.SourcePath); err != nil {
		return nil, err
	}

	before, err := h.Store.GetInventorySource(ctx, req.InventoryId)
	hadSource := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	// Only references that change need the use role, so an inventory's
	// admin can edit its source without holding use on all of it.
	if !hadSource || before.ProjectID != f.ProjectID {
		if err := h.authorizeUse(ctx, rbac.Project, f.ProjectID); err != nil {
			return nil, err
		}
	}
	if _, err := h.Store.GetExecutionEnvironment(ctx, f.ExecutionEnvironmentID); errors.Is(err, store.ErrNotFound) {
		return nil, badRequest("execution environment %s does not exist", f.ExecutionEnvironmentID)
	} else if err != nil {
		return nil, err
	}
	if err := h.checkSyncCredentials(ctx, before.CredentialIDs, f.CredentialIDs); err != nil {
		return nil, err
	}
	// The sync runs the source's code with the credentials, so changing
	// that code needs use on the credentials it keeps too. So does changing
	// the credentials: they share the sync's environment, where one's
	// inputs can decide where another's secrets go (see changesWhatRuns).
	if hadSource && (before.ProjectID != f.ProjectID || before.SourcePath != f.SourcePath ||
		before.ExecutionEnvironmentID != f.ExecutionEnvironmentID || !sameIDs(before.CredentialIDs, f.CredentialIDs)) {
		for _, id := range f.CredentialIDs {
			if err := h.authorize(ctx, rbac.Credential, id, rbac.Use); err != nil {
				if ae := (*apiError)(nil); !errors.As(err, &ae) {
					return nil, err
				}
				return nil, forbidden("Changing the source's project, path, execution environment or credentials needs the use role " +
					"on all the credentials it will have, since the sync runs with them; ask a credential's admin, " +
					"or remove the credentials you can't use")
			}
		}
	}

	src, err := h.Store.SetInventorySource(ctx, req.InventoryId, f)
	if errors.Is(err, store.ErrInvalidReference) {
		return nil, badRequest("the project, execution environment or a credential does not exist")
	}
	if err != nil {
		return nil, err
	}
	out := toAPIInventorySource(src)
	rec := auditRecord{
		Action: apigen.Update, ResourceType: apigen.AuditResourceTypeInventory, ResourceID: inv.ID, ResourceName: inv.Name,
		After: map[string]any{"source": sourceAudit(out)},
	}
	if hadSource {
		rec.Before = map[string]any{"source": sourceAudit(toAPIInventorySource(before))}
	} else {
		rec.Before = map[string]any{"source": nil}
	}
	h.audit(ctx, rec)
	return apigen.SetInventorySource200JSONResponse(out), nil
}

// checkSyncCredentials checks the credentials of a source: the caller needs
// use on those it adds, each must be of a kind a sync can use, and there is
// at most one vault password per vault ID.
func (h *Handlers) checkSyncCredentials(ctx context.Context, old, ids []uuid.UUID) error {
	for _, id := range ids {
		if !containsUUID(old, id) {
			if err := h.authorizeUse(ctx, rbac.Credential, id); err != nil {
				return err
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	creds, err := h.Store.GetCredentials(ctx, ids)
	if errors.Is(err, store.ErrNotFound) {
		return badRequest("a credential in credentialIds does not exist")
	}
	if err != nil {
		return err
	}
	if err := checkGenericEnvConflicts(creds); err != nil {
		return err
	}
	vaultIDs := map[string]string{}
	for _, c := range creds {
		if !syncCredentialKinds[c.Kind] {
			return badRequest("credential %q is a %s credential; an inventory sync can use generic, vault and api_token credentials",
				c.Name, c.Kind)
		}
		if c.Kind == "vault" {
			id := c.PublicInputs["vault_id"]
			if other, ok := vaultIDs[id]; ok {
				return badRequest("credentials %q and %q are both vault passwords for the same vault ID; use one", other, c.Name)
			}
			vaultIDs[id] = c.Name
		}
	}
	return nil
}

// sourceAudit is a source as the audit log records it: its settings, not
// its sync state.
func sourceAudit(s apigen.InventorySource) map[string]any {
	m := toMap(s)
	for _, k := range []string{"lastJobId", "lastJobStatus", "lastSyncedAt", "createdAt", "updatedAt", "inventoryId"} {
		delete(m, k)
	}
	return m
}

func (h *Handlers) DeleteInventorySource(ctx context.Context, req apigen.DeleteInventorySourceRequestObject) (apigen.DeleteInventorySourceResponseObject, error) {
	if err := h.authorize(ctx, rbac.Inventory, req.InventoryId, rbac.Admin); err != nil {
		return nil, err
	}
	inv, err := h.Store.GetInventory(ctx, req.InventoryId)
	if err != nil {
		return nil, orNotFound(err, "Inventory")
	}
	src, err := h.Store.GetInventorySource(ctx, req.InventoryId)
	if err != nil {
		return nil, orNotFound(err, "Inventory source")
	}
	if err := h.Store.DeleteInventorySource(ctx, req.InventoryId); err != nil {
		return nil, orNotFound(err, "Inventory source")
	}
	h.audit(ctx, auditRecord{
		Action: apigen.Update, ResourceType: apigen.AuditResourceTypeInventory, ResourceID: inv.ID, ResourceName: inv.Name,
		Before: map[string]any{"source": sourceAudit(toAPIInventorySource(src))}, After: map[string]any{"source": nil},
	})
	return apigen.DeleteInventorySource204Response{}, nil
}

func (h *Handlers) SyncInventory(ctx context.Context, req apigen.SyncInventoryRequestObject) (apigen.SyncInventoryResponseObject, error) {
	if err := h.authorize(ctx, rbac.Inventory, req.InventoryId, rbac.Admin); err != nil {
		return nil, err
	}
	p := caller(ctx)
	launchType := "api"
	if p.Method == auth.MethodSession {
		launchType = "manual"
	}
	job, err := h.launchSync(ctx, req.InventoryId, store.NewSync{LaunchType: launchType, LaunchedBy: &p.UserID})
	if err != nil {
		return nil, err
	}
	return apigen.SyncInventory201JSONResponse(toAPIJob(job)), nil
}

// launchSync queues a sync of an inventory and audits the launch.
func (h *Handlers) launchSync(ctx context.Context, inventoryID uuid.UUID, in store.NewSync) (store.Job, error) {
	job, err := h.Store.LaunchInventorySync(ctx, inventoryID, in)
	var le *store.LaunchError
	switch {
	case errors.Is(err, store.ErrSyncActive):
		return job, conflict("%s", "A sync of this inventory is already pending or running")
	case errors.As(err, &le):
		return job, badRequest("%s", le.Msg)
	case errors.Is(err, store.ErrNotFound):
		return job, badRequest("the inventory's source refers to a project or execution environment that no longer exists")
	case err != nil:
		return job, err
	}
	p := caller(ctx)
	h.Log.InfoContext(ctx, "inventory sync launched", "job_id", job.ID, "inventory_id", inventoryID, "by", p.Username)
	after := map[string]any{"syncInventoryId": inventoryID, "launchType": in.LaunchType}
	if in.RelaunchOf != nil {
		after["relaunchOf"] = *in.RelaunchOf
	}
	h.audit(ctx, auditRecord{
		Action: apigen.Launch, ResourceType: apigen.AuditResourceTypeJob, ResourceID: job.ID, ResourceName: job.Name,
		After: after,
	})
	return job, nil
}

// PostRunnerInventory replaces the inventory a running sync job syncs
// with what the job found.
func (h *Handlers) PostRunnerInventory(ctx context.Context, req apigen.PostRunnerInventoryRequestObject) (apigen.PostRunnerInventoryResponseObject, error) {
	if err := runnerJob(ctx, req.JobId); err != nil {
		return nil, err
	}
	job, err := h.Store.GetJob(ctx, req.JobId)
	if err != nil {
		return nil, err
	}
	if job.Config.JobType != store.JobTypeInventorySync {
		return nil, conflict("only inventory_sync jobs report an inventory")
	}
	if job.Status != store.JobRunning || job.SyncInventoryID == nil {
		return nil, conflict("the job is not running, or its inventory no longer exists")
	}
	if req.Body == nil {
		return nil, badRequest("the inventory is missing")
	}
	inv, err := store.ParseAnsibleInventory(*req.Body)
	if err != nil {
		return nil, badRequest("%s", err.Error())
	}
	counts, err := h.Store.ReplaceInventoryContents(ctx, job.ID, *job.SyncInventoryID, inv)
	if errors.Is(err, store.ErrJobState) || errors.Is(err, store.ErrNotFound) {
		return nil, conflict("the job is not running, or its inventory no longer exists")
	}
	if err != nil {
		return nil, fmt.Errorf("replace inventory %s: %w", *job.SyncInventoryID, err)
	}
	h.Log.InfoContext(ctx, "inventory synced", "job_id", job.ID, "inventory_id", *job.SyncInventoryID,
		"hosts", len(inv.Hosts), "groups", len(inv.Groups))
	return apigen.PostRunnerInventory200JSONResponse{
		HostsAdded: counts.HostsAdded, HostsUpdated: counts.HostsUpdated, HostsRemoved: counts.HostsRemoved,
		GroupsAdded: counts.GroupsAdded, GroupsUpdated: counts.GroupsUpdated, GroupsRemoved: counts.GroupsRemoved,
		Warnings: nonNilSlice(inv.Warnings),
	}, nil
}

func containsUUID(ids []uuid.UUID, id uuid.UUID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func dedupe(ids []uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if !containsUUID(out, id) {
			out = append(out, id)
		}
	}
	return out
}
