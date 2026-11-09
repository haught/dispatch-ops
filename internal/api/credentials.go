package api

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/google/uuid"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/credentials"
	"dispatch-ops/internal/rbac"
	"dispatch-ops/internal/store"
)

func (h *Handlers) ListCredentialKinds(_ context.Context, _ apigen.ListCredentialKindsRequestObject) (apigen.ListCredentialKindsResponseObject, error) {
	out := apigen.ListCredentialKinds200JSONResponse{Items: make([]apigen.CredentialKindInfo, 0, len(credentials.Kinds))}
	for _, k := range credentials.Kinds {
		info := apigen.CredentialKindInfo{
			Kind: apigen.CredentialKind(k.Name), Label: k.Label, Freeform: k.Freeform,
			Fields: make([]apigen.CredentialField, 0, len(k.Fields)),
		}
		for _, f := range k.Fields {
			info.Fields = append(info.Fields, apigen.CredentialField{
				Name: f.Name, Label: f.Label, Secret: f.Secret, Required: f.Required, Multiline: f.Multiline,
				Destination: f.Destination,
			})
		}
		out.Items = append(out.Items, info)
	}
	return out, nil
}

func (h *Handlers) ListCredentials(ctx context.Context, req apigen.ListCredentialsRequestObject) (apigen.ListCredentialsResponseObject, error) {
	p := req.Params
	items, total, err := h.Store.ListCredentials(ctx,
		store.CredentialFilter{Search: deref(p.Search), Kind: string(deref(p.Kind)), ReadableBy: readableBy(ctx)},
		pageOf(p.Limit, p.Offset))
	if err != nil {
		return nil, err
	}
	out := apigen.ListCredentials200JSONResponse{Items: make([]apigen.Credential, 0, len(items)), Total: total}
	for _, c := range items {
		out.Items = append(out.Items, toAPICredential(c))
	}
	return out, nil
}

func (h *Handlers) CreateCredential(ctx context.Context, req apigen.CreateCredentialRequestObject) (apigen.CreateCredentialResponseObject, error) {
	if err := h.authorizeCreate(ctx, rbac.Credential); err != nil {
		return nil, err
	}
	b := req.Body
	c := store.Credential{ID: uuid.New(), Name: b.Name, Description: deref(b.Description), Kind: string(b.Kind)}
	if p := auth.FromContext(ctx); p != nil {
		c.CreatedBy = &p.UserID
	}
	if err := h.Vault.Seal(&c, b.Inputs); err != nil {
		return nil, err
	}
	c, err := h.Store.CreateCredential(ctx, c)
	if err != nil {
		return nil, err
	}
	if err := h.grantCreator(ctx, rbac.Credential, c.ID); err != nil {
		return nil, err
	}
	out := toAPICredential(c)
	h.Log.InfoContext(ctx, "credential created", "credential_id", c.ID, "kind", c.Kind, "by", principalName(ctx))
	h.audit(ctx, auditRecord{
		Action: apigen.Create, ResourceType: apigen.AuditResourceTypeCredential, ResourceID: c.ID, ResourceName: c.Name,
		After: out,
	})
	return apigen.CreateCredential201JSONResponse(out), nil
}

func (h *Handlers) GetCredential(ctx context.Context, req apigen.GetCredentialRequestObject) (apigen.GetCredentialResponseObject, error) {
	if err := h.authorize(ctx, rbac.Credential, req.CredentialId, rbac.Read); err != nil {
		return nil, err
	}
	c, err := h.Store.GetCredential(ctx, req.CredentialId)
	if err != nil {
		return nil, orNotFound(err, "Credential")
	}
	return apigen.GetCredential200JSONResponse(toAPICredential(c)), nil
}

func (h *Handlers) UpdateCredential(ctx context.Context, req apigen.UpdateCredentialRequestObject) (apigen.UpdateCredentialResponseObject, error) {
	if err := h.authorize(ctx, rbac.Credential, req.CredentialId, rbac.Admin); err != nil {
		return nil, err
	}
	b := req.Body
	var before apigen.Credential
	c, err := h.Store.UpdateCredential(ctx, req.CredentialId, func(c *store.Credential) error {
		before = toAPICredential(*c)
		setIf(&c.Name, b.Name)
		setIf(&c.Description, b.Description)
		if b.Inputs == nil {
			return nil
		}
		if kind, ok := credentials.Lookup(c.Kind); ok {
			if err := kind.CheckDestinationChange(c.PublicInputs, c.SecretFields, *b.Inputs); err != nil {
				return err
			}
		}
		inputs, err := h.Vault.Inputs(*c)
		if err != nil {
			return err
		}
		for name, v := range *b.Inputs {
			if v == "" {
				delete(inputs, name)
			} else {
				inputs[name] = v
			}
		}
		// Re-encrypting also moves the value to the current key.
		return h.Vault.Seal(c, inputs)
	})
	if err != nil {
		return nil, orNotFound(err, "Credential")
	}
	out := toAPICredential(c)
	after := toMap(out)
	if b.Inputs != nil {
		// Secret values never appear in the log, only which were changed.
		after["changedInputs"] = slices.Sorted(maps.Keys(*b.Inputs))
	}
	h.Log.InfoContext(ctx, "credential updated", "credential_id", c.ID, "inputs_changed", b.Inputs != nil, "by", principalName(ctx))
	h.audit(ctx, auditRecord{
		Action: apigen.Update, ResourceType: apigen.AuditResourceTypeCredential, ResourceID: c.ID, ResourceName: c.Name,
		Before: before, After: after,
	})
	return apigen.UpdateCredential200JSONResponse(out), nil
}

func (h *Handlers) DeleteCredential(ctx context.Context, req apigen.DeleteCredentialRequestObject) (apigen.DeleteCredentialResponseObject, error) {
	if err := h.authorize(ctx, rbac.Credential, req.CredentialId, rbac.Admin); err != nil {
		return nil, err
	}
	c, err := h.Store.GetCredential(ctx, req.CredentialId)
	if err != nil {
		return nil, orNotFound(err, "Credential")
	}
	if err := h.Store.DeleteCredential(ctx, req.CredentialId); err != nil {
		return nil, orNotFound(err, "Credential")
	}
	h.Log.InfoContext(ctx, "credential deleted", "credential_id", req.CredentialId, "by", principalName(ctx))
	h.audit(ctx, auditRecord{
		Action: apigen.Delete, ResourceType: apigen.AuditResourceTypeCredential, ResourceID: c.ID, ResourceName: c.Name,
		Before: toAPICredential(c),
	})
	return apigen.DeleteCredential204Response{}, nil
}

func toAPICredential(c store.Credential) apigen.Credential {
	public := maps.Clone(c.PublicInputs)
	if public == nil {
		public = map[string]string{}
	}
	return apigen.Credential{
		Id: c.ID, Name: c.Name, Description: c.Description, Kind: apigen.CredentialKind(c.Kind),
		PublicInputs: public, SecretFields: nonNilSlice(c.SecretFields), CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

// --- Execution environments ---

// checkRegistryCredential verifies that id names a registry credential.
func (h *Handlers) checkRegistryCredential(ctx context.Context, id *uuid.UUID) error {
	if id == nil {
		return nil
	}
	c, err := h.Store.GetCredential(ctx, *id)
	if errors.Is(err, store.ErrNotFound) {
		return badRequest("registry credential %s does not exist", id)
	}
	if err != nil {
		return err
	}
	if c.Kind != "registry" {
		return badRequest("credential %q is a %s credential; image pulls need a registry credential", c.Name, c.Kind)
	}
	return nil
}

func (h *Handlers) ListExecutionEnvironments(ctx context.Context, req apigen.ListExecutionEnvironmentsRequestObject) (apigen.ListExecutionEnvironmentsResponseObject, error) {
	p := req.Params
	items, total, err := h.Store.ListExecutionEnvironments(ctx, deref(p.Search), pageOf(p.Limit, p.Offset))
	if err != nil {
		return nil, err
	}
	out := apigen.ListExecutionEnvironments200JSONResponse{Items: make([]apigen.ExecutionEnvironment, 0, len(items)), Total: total}
	for _, e := range items {
		out.Items = append(out.Items, toAPIExecutionEnvironment(e))
	}
	return out, nil
}

func (h *Handlers) CreateExecutionEnvironment(ctx context.Context, req apigen.CreateExecutionEnvironmentRequestObject) (apigen.CreateExecutionEnvironmentResponseObject, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	b := req.Body
	if err := h.checkRegistryCredential(ctx, b.RegistryCredentialId); err != nil {
		return nil, err
	}
	pull := apigen.IfNotPresent
	setIf(&pull, b.PullPolicy)
	e, err := h.Store.CreateExecutionEnvironment(ctx, store.ExecutionEnvironmentFields{
		Name: b.Name, Description: deref(b.Description), Image: b.Image, PullPolicy: string(pull),
		RegistryCredentialID: b.RegistryCredentialId,
	})
	if err != nil {
		return nil, err
	}
	out := toAPIExecutionEnvironment(e)
	h.audit(ctx, auditRecord{
		Action: apigen.Create, ResourceType: apigen.AuditResourceTypeExecutionEnvironment, ResourceID: e.ID, ResourceName: e.Name,
		After: out,
	})
	return apigen.CreateExecutionEnvironment201JSONResponse(out), nil
}

func (h *Handlers) GetExecutionEnvironment(ctx context.Context, req apigen.GetExecutionEnvironmentRequestObject) (apigen.GetExecutionEnvironmentResponseObject, error) {
	e, err := h.Store.GetExecutionEnvironment(ctx, req.ExecutionEnvironmentId)
	if err != nil {
		return nil, orNotFound(err, "Execution environment")
	}
	return apigen.GetExecutionEnvironment200JSONResponse(toAPIExecutionEnvironment(e)), nil
}

func (h *Handlers) UpdateExecutionEnvironment(ctx context.Context, req apigen.UpdateExecutionEnvironmentRequestObject) (apigen.UpdateExecutionEnvironmentResponseObject, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	b := req.Body
	var regCred *uuid.UUID
	if b.RegistryCredentialId.IsSpecified() && !b.RegistryCredentialId.IsNull() {
		id := b.RegistryCredentialId.MustGet()
		regCred = &id
		if err := h.checkRegistryCredential(ctx, regCred); err != nil {
			return nil, err
		}
	}
	var before apigen.ExecutionEnvironment
	e, err := h.Store.UpdateExecutionEnvironment(ctx, req.ExecutionEnvironmentId, func(f *store.ExecutionEnvironmentFields) error {
		before = toAPIExecutionEnvironment(store.ExecutionEnvironment{
			ID: req.ExecutionEnvironmentId, Name: f.Name, Description: f.Description, Image: f.Image,
			PullPolicy: f.PullPolicy, RegistryCredentialID: f.RegistryCredentialID,
		})
		setIf(&f.Name, b.Name)
		setIf(&f.Description, b.Description)
		setIf(&f.Image, b.Image)
		if b.PullPolicy != nil {
			f.PullPolicy = string(*b.PullPolicy)
		}
		if b.RegistryCredentialId.IsSpecified() {
			f.RegistryCredentialID = regCred
		}
		return nil
	})
	if err != nil {
		return nil, orNotFound(err, "Execution environment")
	}
	out := toAPIExecutionEnvironment(e)
	before.CreatedAt = out.CreatedAt
	h.audit(ctx, auditRecord{
		Action: apigen.Update, ResourceType: apigen.AuditResourceTypeExecutionEnvironment, ResourceID: e.ID, ResourceName: e.Name,
		Before: before, After: out,
	})
	return apigen.UpdateExecutionEnvironment200JSONResponse(out), nil
}

func (h *Handlers) DeleteExecutionEnvironment(ctx context.Context, req apigen.DeleteExecutionEnvironmentRequestObject) (apigen.DeleteExecutionEnvironmentResponseObject, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	e, err := h.Store.GetExecutionEnvironment(ctx, req.ExecutionEnvironmentId)
	if err != nil {
		return nil, orNotFound(err, "Execution environment")
	}
	if err := h.Store.DeleteExecutionEnvironment(ctx, req.ExecutionEnvironmentId); err != nil {
		return nil, orNotFound(err, "Execution environment")
	}
	h.audit(ctx, auditRecord{
		Action: apigen.Delete, ResourceType: apigen.AuditResourceTypeExecutionEnvironment, ResourceID: e.ID, ResourceName: e.Name,
		Before: toAPIExecutionEnvironment(e),
	})
	return apigen.DeleteExecutionEnvironment204Response{}, nil
}

func toAPIExecutionEnvironment(e store.ExecutionEnvironment) apigen.ExecutionEnvironment {
	return apigen.ExecutionEnvironment{
		Id: e.ID, Name: e.Name, Description: e.Description, Image: e.Image, PullPolicy: apigen.PullPolicy(e.PullPolicy),
		RegistryCredentialId: e.RegistryCredentialID, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
}

// principalName names the caller for log lines.
func principalName(ctx context.Context) string {
	if p := auth.FromContext(ctx); p != nil {
		return p.Username
	}
	return ""
}
