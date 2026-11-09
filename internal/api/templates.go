package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/google/uuid"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/hostpattern"
	"dispatch-ops/internal/rbac"
	"dispatch-ops/internal/store"
)

// checkTemplate checks that a template's fields fit its job type.
func checkTemplate(f store.JobTemplateFields) error {
	t := f.JobType
	overrides := func(field apigen.OverridableField) bool { return slices.Contains(f.AllowOverrides, string(field)) }
	switch t {
	case string(apigen.Container):
		if f.ProjectID != nil || f.Entrypoint != "" {
			return badRequest("container templates run a command; projectId and entrypoint are for ansible and script templates")
		}
		if len(f.Command) == 0 {
			return badRequest("container templates need a command")
		}
		if len(f.Arguments) > 0 {
			return badRequest("arguments are for script templates; put a container's arguments in its command")
		}
	case string(apigen.Ansible), string(apigen.Script):
		what := map[string]string{"ansible": "playbook", "script": "script"}[t]
		if f.ProjectID == nil {
			return badRequest("%s templates need a projectId", t)
		}
		if f.Entrypoint == "" {
			return badRequest("%s templates need an entrypoint: the %s's path in the project", t, what)
		}
		if !filepath.IsLocal(f.Entrypoint) || strings.ContainsRune(f.Entrypoint, '\\') {
			return badRequest("entrypoint must be a relative path inside the project, like dir/file, without ..")
		}
		if len(f.Command) > 0 || overrides(apigen.OverridableFieldCommand) {
			return badRequest("command is for container templates; %s templates run their entrypoint", t)
		}
		if t == string(apigen.Ansible) && len(f.Arguments) > 0 {
			return badRequest("arguments are for script templates; pass variables to a playbook with extraVars")
		}
	case string(apigen.InventorySync):
		return badRequest("inventory_sync jobs are launched by syncing an inventory, not from a job template")
	default:
		return badRequest("unknown job type %q", t)
	}
	if err := hostpattern.CheckLimit(f.Limit); err != nil {
		return badRequest("%s", err)
	}
	if t != string(apigen.Ansible) {
		if f.Tags != "" || f.SkipTags != "" || f.Verbosity != 0 ||
			overrides(apigen.OverridableFieldTags) || overrides(apigen.OverridableFieldSkipTags) {
			return badRequest("tags, skipTags and verbosity are for ansible templates")
		}
	}
	return nil
}

// checkTemplateCredentials checks the credentials a template of jobType
// uses. No two generic credentials may set the same environment variable
// (the runner refuses such jobs too). A playbook runs with at most one
// machine credential (ssh or username_password) and one vault password per
// vault ID.
func (h *Handlers) checkTemplateCredentials(ctx context.Context, jobType string, ids []uuid.UUID) error {
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
	if jobType != string(apigen.Ansible) {
		return nil
	}
	var machine []string
	vaultIDs := map[string]string{}
	for _, c := range creds {
		switch c.Kind {
		case "ssh", "username_password":
			machine = append(machine, fmt.Sprintf("%q", c.Name))
		case "vault":
			id := c.PublicInputs["vault_id"]
			if other, ok := vaultIDs[id]; ok {
				label := "without a vault ID"
				if id != "" {
					label = fmt.Sprintf("with vault ID %q", id)
				}
				return badRequest("credentials %q and %q are both vault passwords %s; use one", other, c.Name, label)
			}
			vaultIDs[id] = c.Name
		}
	}
	if len(machine) > 1 {
		return badRequest("a playbook connects to hosts with one machine credential (ssh or username_password); got %s",
			strings.Join(machine, ", "))
	}
	return nil
}

// checkGenericEnvConflicts refuses generic credentials that set the same
// environment variable, which the runner refuses too: one would silently
// override the other, deciding where its secrets go. Names are compared
// ignoring case, like the runner does, since some tools read either case.
func checkGenericEnvConflicts(creds []store.Credential) error {
	setBy := map[string]store.Credential{}
	for _, c := range creds {
		if c.Kind != "generic" {
			continue
		}
		names := slices.Concat(c.SecretFields, slices.Collect(maps.Keys(c.PublicInputs)))
		slices.Sort(names)
		for _, name := range names {
			key := strings.ToUpper(name)
			if other, ok := setBy[key]; ok && other.ID != c.ID {
				return badRequest("credentials %q and %q both set %s; a job's credentials can't set the same environment variable",
					other.Name, c.Name, name)
			}
			setBy[key] = c
		}
	}
	return nil
}

// checkTemplateRefs checks that the caller may use the project, inventory
// and credentials a template references. With old set, only references
// that change are checked, so a template's admin can edit it without being
// able to use everything it already references, except that a change to
// what the job runs, or where, or with which credentials, needs use on all
// the credentials it will have (see changesWhatRuns). Credentials it stops
// referencing need nothing: the job no longer gets them.
func (h *Handlers) checkTemplateRefs(ctx context.Context, old *store.JobTemplate, f store.JobTemplateFields) error {
	if old != nil {
		if what := changesWhatRuns(*old, f); what != "" {
			for _, id := range f.CredentialIDs {
				if err := h.authorize(ctx, rbac.Credential, id, rbac.Use); err != nil {
					if ae := (*apiError)(nil); !errors.As(err, &ae) {
						return err
					}
					return forbidden(fmt.Sprintf("Changing the template's %s needs the use role on all its credentials, "+
						"since the job runs with them; ask a credential's admin, or remove the credentials you can't use", what))
				}
			}
		}
	}
	if f.ProjectID != nil && (old == nil || old.ProjectID == nil || *old.ProjectID != *f.ProjectID) {
		if err := h.authorizeUse(ctx, rbac.Project, *f.ProjectID); err != nil {
			return err
		}
	}
	if f.InventoryID != nil && (old == nil || old.InventoryID == nil || *old.InventoryID != *f.InventoryID) {
		if err := h.authorizeUse(ctx, rbac.Inventory, *f.InventoryID); err != nil {
			return err
		}
	}
	for _, id := range f.CredentialIDs {
		if old == nil || !slices.Contains(old.CredentialIDs, id) {
			if err := h.authorizeUse(ctx, rbac.Credential, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// credentialOverrides are the launch overrides that let a launcher change
// what a job runs or where, and so reach its credentials (a launch-time
// inventory brings its own host vars, for one, and tags or a limit can skip
// a playbook's guards), matching the fields changesWhatRuns gates.
var credentialOverrides = []string{
	string(apigen.OverridableFieldCommand), string(apigen.OverridableFieldExtraVars), string(apigen.OverridableFieldInventory),
	string(apigen.OverridableFieldTags), string(apigen.OverridableFieldSkipTags), string(apigen.OverridableFieldLimit),
}

// changesWhatRuns names a field whose change from old to f changes the
// code a template's jobs run, or the hosts they send credentials to, or
// returns "". Whoever makes such a change could make the job hand over its
// credentials, so it needs the use role on them.
func changesWhatRuns(old store.JobTemplate, f store.JobTemplateFields) string {
	ptrEq := func(a, b *uuid.UUID) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	switch {
	case !slices.Equal(old.Command, f.Command):
		return "command"
	case old.Entrypoint != f.Entrypoint:
		return "entrypoint"
	case !slices.Equal(old.Arguments, f.Arguments):
		return "arguments"
	case !reflect.DeepEqual(nonNilVars(old.ExtraVars), nonNilVars(f.ExtraVars)):
		return "extra vars"
	case old.ExecutionEnvironmentID != f.ExecutionEnvironmentID:
		return "execution environment"
	case !ptrEq(old.ProjectID, f.ProjectID):
		return "project"
	case !ptrEq(old.InventoryID, f.InventoryID):
		return "inventory"
	case old.Verbosity != f.Verbosity:
		// High verbosity prints module arguments and results, which may
		// hold the credentials' secrets.
		return "verbosity"
	case old.Tags != f.Tags:
		// Running only some tasks can skip the checks a playbook guards
		// its credentials with, like skipping tags.
		return "tags"
	case old.SkipTags != f.SkipTags:
		// Skipping tasks can skip the checks a playbook guards its
		// credentials with.
		return "skip tags"
	case old.Limit != f.Limit:
		// A limit can skip whole plays, like a first play that checks, on
		// localhost, what a later one does on the hosts.
		return "limit"
	case !sameIDs(old.CredentialIDs, f.CredentialIDs):
		// Credentials share the job's environment: one's inputs can decide
		// where another's secrets go (VAULT_ADDR beside VAULT_TOKEN), and one
		// can hold settings a playbook guards another with, so adding or
		// removing one changes what the others do.
		return "credentials"
	}
	for _, o := range credentialOverrides {
		if slices.Contains(f.AllowOverrides, o) && !slices.Contains(old.AllowOverrides, o) {
			return "launch overrides (" + o + ")"
		}
	}
	return ""
}

// sameIDs reports whether a and b hold the same IDs, in any order.
func sameIDs(a, b []uuid.UUID) bool {
	for _, id := range a {
		if !slices.Contains(b, id) {
			return false
		}
	}
	for _, id := range b {
		if !slices.Contains(a, id) {
			return false
		}
	}
	return true
}

func overridesToStrings(o *[]apigen.OverridableField) []string {
	if o == nil {
		return []string{}
	}
	out := make([]string, len(*o))
	for i, f := range *o {
		out[i] = string(f)
	}
	return out
}

func (h *Handlers) ListJobTemplates(ctx context.Context, req apigen.ListJobTemplatesRequestObject) (apigen.ListJobTemplatesResponseObject, error) {
	p := req.Params
	items, total, err := h.Store.ListJobTemplates(ctx, deref(p.Search), readableBy(ctx), pageOf(p.Limit, p.Offset))
	if err != nil {
		return nil, err
	}
	out := apigen.ListJobTemplates200JSONResponse{Items: make([]apigen.JobTemplate, 0, len(items)), Total: total}
	for _, t := range items {
		out.Items = append(out.Items, toAPIJobTemplate(t))
	}
	return out, nil
}

func (h *Handlers) CreateJobTemplate(ctx context.Context, req apigen.CreateJobTemplateRequestObject) (apigen.CreateJobTemplateResponseObject, error) {
	if err := h.authorizeCreate(ctx, rbac.JobTemplate); err != nil {
		return nil, err
	}
	b := req.Body
	timeout := 3600
	setIf(&timeout, b.TimeoutSeconds)
	f := store.JobTemplateFields{
		Name: b.Name, Description: deref(b.Description), JobType: string(b.JobType),
		ExecutionEnvironmentID: b.ExecutionEnvironmentId, ProjectID: b.ProjectId, Entrypoint: deref(b.Entrypoint),
		InventoryID: b.InventoryId, CredentialIDs: deref(b.CredentialIds), Command: deref(b.Command),
		Arguments: deref(b.Arguments), ExtraVars: vars(b.ExtraVars), Limit: deref(b.Limit),
		Tags: deref(b.Tags), SkipTags: deref(b.SkipTags), Verbosity: deref(b.Verbosity),
		TimeoutSeconds: timeout, AllowOverrides: overridesToStrings(b.AllowOverrides),
	}
	if err := checkTemplate(f); err != nil {
		return nil, err
	}
	if err := h.checkTemplateRefs(ctx, nil, f); err != nil {
		return nil, err
	}
	if err := h.checkTemplateCredentials(ctx, f.JobType, f.CredentialIDs); err != nil {
		return nil, err
	}
	t, err := h.Store.CreateJobTemplate(ctx, f)
	if err != nil {
		return nil, err
	}
	if err := h.grantCreator(ctx, rbac.JobTemplate, t.ID); err != nil {
		return nil, err
	}
	out := toAPIJobTemplate(t)
	h.audit(ctx, auditRecord{
		Action: apigen.Create, ResourceType: apigen.AuditResourceTypeJobTemplate, ResourceID: t.ID, ResourceName: t.Name,
		After: out,
	})
	return apigen.CreateJobTemplate201JSONResponse(out), nil
}

func (h *Handlers) GetJobTemplate(ctx context.Context, req apigen.GetJobTemplateRequestObject) (apigen.GetJobTemplateResponseObject, error) {
	if err := h.authorize(ctx, rbac.JobTemplate, req.JobTemplateId, rbac.Read); err != nil {
		return nil, err
	}
	t, err := h.Store.GetJobTemplate(ctx, req.JobTemplateId)
	if err != nil {
		return nil, orNotFound(err, "Job template")
	}
	return apigen.GetJobTemplate200JSONResponse(toAPIJobTemplate(t)), nil
}

func (h *Handlers) UpdateJobTemplate(ctx context.Context, req apigen.UpdateJobTemplateRequestObject) (apigen.UpdateJobTemplateResponseObject, error) {
	if err := h.authorize(ctx, rbac.JobTemplate, req.JobTemplateId, rbac.Admin); err != nil {
		return nil, err
	}
	b := req.Body
	var before store.JobTemplate
	t, err := h.Store.UpdateJobTemplate(ctx, req.JobTemplateId, func(f *store.JobTemplateFields) error {
		before = store.JobTemplate{
			ID: req.JobTemplateId, Name: f.Name, Description: f.Description, JobType: f.JobType,
			ExecutionEnvironmentID: f.ExecutionEnvironmentID, ProjectID: f.ProjectID, Entrypoint: f.Entrypoint,
			InventoryID: f.InventoryID, CredentialIDs: slices.Clone(f.CredentialIDs), Command: f.Command,
			Arguments: f.Arguments, ExtraVars: maps.Clone(f.ExtraVars), Limit: f.Limit, Tags: f.Tags,
			SkipTags: f.SkipTags, Verbosity: f.Verbosity, TimeoutSeconds: f.TimeoutSeconds, AllowOverrides: f.AllowOverrides,
		}
		setIf(&f.Name, b.Name)
		setIf(&f.Description, b.Description)
		setIf(&f.ExecutionEnvironmentID, b.ExecutionEnvironmentId)
		if b.ProjectId != nil {
			f.ProjectID = b.ProjectId
		}
		setIf(&f.Entrypoint, b.Entrypoint)
		setIf(&f.CredentialIDs, b.CredentialIds)
		setIf(&f.Command, b.Command)
		setIf(&f.Arguments, b.Arguments)
		setIf(&f.Limit, b.Limit)
		setIf(&f.Tags, b.Tags)
		setIf(&f.SkipTags, b.SkipTags)
		setIf(&f.Verbosity, b.Verbosity)
		setIf(&f.TimeoutSeconds, b.TimeoutSeconds)
		if b.ExtraVars != nil {
			f.ExtraVars = *b.ExtraVars
		}
		if b.AllowOverrides != nil {
			f.AllowOverrides = overridesToStrings(b.AllowOverrides)
		}
		if b.InventoryId.IsSpecified() {
			f.InventoryID = nil
			if !b.InventoryId.IsNull() {
				id := b.InventoryId.MustGet()
				f.InventoryID = &id
			}
		}
		if err := checkTemplate(*f); err != nil {
			return err
		}
		if err := h.checkTemplateRefs(ctx, &before, *f); err != nil {
			return err
		}
		if b.CredentialIds != nil {
			return h.checkTemplateCredentials(ctx, f.JobType, f.CredentialIDs)
		}
		return nil
	})
	if err != nil {
		return nil, orNotFound(err, "Job template")
	}
	out := toAPIJobTemplate(t)
	before.CreatedAt = t.CreatedAt
	h.audit(ctx, auditRecord{
		Action: apigen.Update, ResourceType: apigen.AuditResourceTypeJobTemplate, ResourceID: t.ID, ResourceName: t.Name,
		Before: toAPIJobTemplate(before), After: out,
	})
	return apigen.UpdateJobTemplate200JSONResponse(out), nil
}

func (h *Handlers) DeleteJobTemplate(ctx context.Context, req apigen.DeleteJobTemplateRequestObject) (apigen.DeleteJobTemplateResponseObject, error) {
	if err := h.authorize(ctx, rbac.JobTemplate, req.JobTemplateId, rbac.Admin); err != nil {
		return nil, err
	}
	t, err := h.Store.GetJobTemplate(ctx, req.JobTemplateId)
	if err != nil {
		return nil, orNotFound(err, "Job template")
	}
	if err := h.Store.DeleteJobTemplate(ctx, req.JobTemplateId); err != nil {
		return nil, orNotFound(err, "Job template")
	}
	h.audit(ctx, auditRecord{
		Action: apigen.Delete, ResourceType: apigen.AuditResourceTypeJobTemplate, ResourceID: t.ID, ResourceName: t.Name,
		Before: toAPIJobTemplate(t),
	})
	return apigen.DeleteJobTemplate204Response{}, nil
}

func (h *Handlers) LaunchJobTemplate(ctx context.Context, req apigen.LaunchJobTemplateRequestObject) (apigen.LaunchJobTemplateResponseObject, error) {
	if err := h.authorize(ctx, rbac.JobTemplate, req.JobTemplateId, rbac.Execute); err != nil {
		return nil, err
	}
	t, err := h.Store.GetJobTemplate(ctx, req.JobTemplateId)
	if err != nil {
		return nil, orNotFound(err, "Job template")
	}
	var b apigen.LaunchRequest
	if req.Body != nil {
		b = *req.Body
	}
	overrides := launchOverrides(b)
	if err := h.checkOverrideRefs(ctx, t, overrides); err != nil {
		return nil, err
	}
	cfg, err := h.Store.ResolveJobConfig(ctx, t, overrides)
	if err != nil {
		return nil, err
	}

	p := caller(ctx)
	launchType := "api"
	if p.Method == auth.MethodSession {
		launchType = "manual"
	}
	job, err := h.Store.CreateJob(ctx, store.NewJob{
		TemplateID: &t.ID, Name: t.Name, LaunchType: launchType, LaunchedBy: &p.UserID, Config: cfg, Overrides: &overrides,
	})
	if err != nil {
		return nil, err
	}
	h.Log.InfoContext(ctx, "job launched", "job_id", job.ID, "job_template_id", t.ID, "by", p.Username)
	launch := map[string]any{"jobTemplateId": t.ID, "launchType": launchType}
	if o := toMap(b); len(o) > 0 {
		launch["overrides"] = o
	}
	h.audit(ctx, auditRecord{
		Action: apigen.Launch, ResourceType: apigen.AuditResourceTypeJob, ResourceID: job.ID, ResourceName: job.Name,
		After: launch,
	})
	return apigen.LaunchJobTemplate201JSONResponse(toAPIJob(job)), nil
}

// launchOverrides converts a launch request's overrides.
func launchOverrides(b apigen.LaunchRequest) store.LaunchOverrides {
	o := store.LaunchOverrides{InventoryID: b.InventoryId, Limit: b.Limit, Tags: b.Tags, SkipTags: b.SkipTags}
	if b.ExtraVars != nil {
		o.ExtraVars = *b.ExtraVars
	}
	if b.Command != nil {
		o.Command = *b.Command
	}
	return o
}

// toAPILaunchRequest is the reverse of launchOverrides.
func toAPILaunchRequest(o store.LaunchOverrides) apigen.LaunchRequest {
	b := apigen.LaunchRequest{InventoryId: o.InventoryID, Limit: o.Limit, Tags: o.Tags, SkipTags: o.SkipTags}
	if o.ExtraVars != nil {
		v := apigen.Variables(o.ExtraVars)
		b.ExtraVars = &v
	}
	if o.Command != nil {
		c := apigen.Command(o.Command)
		b.Command = &c
	}
	return b
}

// checkOverrideRefs checks that the template allows the overrides and that
// the caller may use an inventory they name.
func (h *Handlers) checkOverrideRefs(ctx context.Context, t store.JobTemplate, o store.LaunchOverrides) error {
	if err := o.CheckAllowed(t); err != nil {
		return err
	}
	if o.InventoryID != nil {
		return h.authorizeUse(ctx, rbac.Inventory, *o.InventoryID)
	}
	return nil
}

func toAPIJobTemplate(t store.JobTemplate) apigen.JobTemplate {
	overrides := make([]apigen.OverridableField, len(t.AllowOverrides))
	for i, o := range t.AllowOverrides {
		overrides[i] = apigen.OverridableField(o)
	}
	return apigen.JobTemplate{
		Id: t.ID, Name: t.Name, Description: t.Description, JobType: apigen.JobType(t.JobType),
		ExecutionEnvironmentId: t.ExecutionEnvironmentID, ProjectId: t.ProjectID, Entrypoint: t.Entrypoint,
		InventoryId: t.InventoryID, CredentialIds: nonNilSlice(t.CredentialIDs), Command: nonNilSlice(t.Command),
		Arguments: nonNilSlice(t.Arguments), ExtraVars: t.ExtraVars, Limit: t.Limit, Tags: t.Tags,
		SkipTags: t.SkipTags, Verbosity: t.Verbosity, TimeoutSeconds: t.TimeoutSeconds, AllowOverrides: overrides,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func toAPIJob(j store.Job) apigen.Job {
	c := j.Config
	out := apigen.Job{
		Id: j.ID, Name: j.Name, JobTemplateId: j.TemplateID, Status: apigen.JobStatus(j.Status),
		LaunchType: apigen.JobLaunchType(j.LaunchType), CancelRequested: j.CancelRequested,
		ExitCode: j.ExitCode, FailureReason: j.FailureReason, ScmRevision: j.ScmRevision,
		CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
		Config: apigen.JobConfig{
			JobType: apigen.JobType(c.JobType),
			ExecutionEnvironment: apigen.ExecutionEnvironmentRef{
				Id: c.ExecutionEnvironment.ID, Name: c.ExecutionEnvironment.Name,
				Image: c.ExecutionEnvironment.Image, PullPolicy: apigen.PullPolicy(c.ExecutionEnvironment.PullPolicy),
			},
			Entrypoint: c.Entrypoint, InventoryId: c.InventoryID, CredentialIds: nonNilSlice(c.CredentialIDs),
			Command: nonNilSlice(c.Command), Arguments: nonNilSlice(c.Arguments), ExtraVars: nonNilVars(c.ExtraVars),
			Limit: c.Limit, Tags: c.Tags, SkipTags: c.SkipTags, Verbosity: c.Verbosity, TimeoutSeconds: c.TimeoutSeconds,
		},
	}
	if p := c.Project; p != nil {
		out.Config.Project = &apigen.ProjectRef{Id: p.ID, Name: p.Name, ScmUrl: redactScmURL(p.ScmURL), ScmRef: p.ScmRef}
	}
	if j.LaunchedBy != nil {
		out.LaunchedBy = &apigen.UserRef{Id: j.LaunchedBy.ID, Username: j.LaunchedBy.Username}
	}
	if j.Schedule != nil {
		out.Schedule = &apigen.ScheduleRef{Id: j.Schedule.ID, Name: j.Schedule.Name}
	}
	out.RelaunchOf = j.RelaunchOf
	out.SyncInventoryId = j.SyncInventoryID
	return out
}

func nonNilVars(m map[string]any) apigen.Variables {
	if m == nil {
		return apigen.Variables{}
	}
	return m
}
