package api

import (
	"context"
	"errors"
	"maps"
	"slices"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/store"
)

// supportedJobTypes are the job types this version can run.
var supportedJobTypes = []apigen.JobType{apigen.Container}

func checkJobType(t apigen.JobType) error {
	if !slices.Contains(supportedJobTypes, t) {
		return badRequest("job type %q is not supported yet; this version runs container jobs", t)
	}
	return nil
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
	items, total, err := h.Store.ListJobTemplates(ctx, deref(p.Search), pageOf(p.Limit, p.Offset))
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
	b := req.Body
	if err := checkJobType(b.JobType); err != nil {
		return nil, err
	}
	timeout := 3600
	setIf(&timeout, b.TimeoutSeconds)
	t, err := h.Store.CreateJobTemplate(ctx, store.JobTemplateFields{
		Name: b.Name, Description: deref(b.Description), JobType: string(b.JobType),
		ExecutionEnvironmentID: b.ExecutionEnvironmentId, InventoryID: b.InventoryId,
		CredentialIDs: deref(b.CredentialIds), Command: b.Command, ExtraVars: vars(b.ExtraVars),
		Limit: deref(b.Limit), TimeoutSeconds: timeout, AllowOverrides: overridesToStrings(b.AllowOverrides),
	})
	if err != nil {
		return nil, err
	}
	return apigen.CreateJobTemplate201JSONResponse(toAPIJobTemplate(t)), nil
}

func (h *Handlers) GetJobTemplate(ctx context.Context, req apigen.GetJobTemplateRequestObject) (apigen.GetJobTemplateResponseObject, error) {
	t, err := h.Store.GetJobTemplate(ctx, req.JobTemplateId)
	if err != nil {
		return nil, orNotFound(err, "Job template")
	}
	return apigen.GetJobTemplate200JSONResponse(toAPIJobTemplate(t)), nil
}

func (h *Handlers) UpdateJobTemplate(ctx context.Context, req apigen.UpdateJobTemplateRequestObject) (apigen.UpdateJobTemplateResponseObject, error) {
	b := req.Body
	t, err := h.Store.UpdateJobTemplate(ctx, req.JobTemplateId, func(f *store.JobTemplateFields) error {
		setIf(&f.Name, b.Name)
		setIf(&f.Description, b.Description)
		setIf(&f.ExecutionEnvironmentID, b.ExecutionEnvironmentId)
		setIf(&f.CredentialIDs, b.CredentialIds)
		setIf(&f.Command, b.Command)
		setIf(&f.Limit, b.Limit)
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
		return nil
	})
	if err != nil {
		return nil, orNotFound(err, "Job template")
	}
	return apigen.UpdateJobTemplate200JSONResponse(toAPIJobTemplate(t)), nil
}

func (h *Handlers) DeleteJobTemplate(ctx context.Context, req apigen.DeleteJobTemplateRequestObject) (apigen.DeleteJobTemplateResponseObject, error) {
	if err := h.Store.DeleteJobTemplate(ctx, req.JobTemplateId); err != nil {
		return nil, orNotFound(err, "Job template")
	}
	return apigen.DeleteJobTemplate204Response{}, nil
}

func (h *Handlers) LaunchJobTemplate(ctx context.Context, req apigen.LaunchJobTemplateRequestObject) (apigen.LaunchJobTemplateResponseObject, error) {
	t, err := h.Store.GetJobTemplate(ctx, req.JobTemplateId)
	if err != nil {
		return nil, orNotFound(err, "Job template")
	}
	if err := checkJobType(apigen.JobType(t.JobType)); err != nil {
		return nil, err
	}
	var b apigen.LaunchRequest
	if req.Body != nil {
		b = *req.Body
	}

	cfg := store.JobConfig{
		JobType: t.JobType, InventoryID: t.InventoryID, CredentialIDs: nonNilSlice(t.CredentialIDs),
		Command: t.Command, ExtraVars: maps.Clone(t.ExtraVars), Limit: t.Limit, TimeoutSeconds: t.TimeoutSeconds,
	}
	if cfg.ExtraVars == nil {
		cfg.ExtraVars = map[string]any{}
	}
	allowed := func(field apigen.OverridableField) error {
		if !slices.Contains(t.AllowOverrides, string(field)) {
			return badRequest("the job template does not allow overriding %q at launch", field)
		}
		return nil
	}
	if b.InventoryId != nil {
		if err := allowed(apigen.OverridableFieldInventory); err != nil {
			return nil, err
		}
		if _, err := h.Store.GetInventory(ctx, *b.InventoryId); errors.Is(err, store.ErrNotFound) {
			return nil, badRequest("inventory %s does not exist", b.InventoryId)
		} else if err != nil {
			return nil, err
		}
		cfg.InventoryID = b.InventoryId
	}
	if b.ExtraVars != nil {
		if err := allowed(apigen.OverridableFieldExtraVars); err != nil {
			return nil, err
		}
		maps.Copy(cfg.ExtraVars, *b.ExtraVars)
	}
	if b.Limit != nil {
		if err := allowed(apigen.OverridableFieldLimit); err != nil {
			return nil, err
		}
		cfg.Limit = *b.Limit
	}
	if b.Command != nil {
		if err := allowed(apigen.OverridableFieldCommand); err != nil {
			return nil, err
		}
		cfg.Command = *b.Command
	}

	ee, err := h.Store.GetExecutionEnvironment(ctx, t.ExecutionEnvironmentID)
	if err != nil {
		return nil, err
	}
	cfg.ExecutionEnvironment = store.JobExecutionEnvironment{
		ID: ee.ID, Name: ee.Name, Image: ee.Image, PullPolicy: ee.PullPolicy, RegistryCredentialID: ee.RegistryCredentialID,
	}

	p := auth.FromContext(ctx)
	launchType := "api"
	if p.Method == auth.MethodSession {
		launchType = "manual"
	}
	job, err := h.Store.CreateJob(ctx, store.NewJob{
		TemplateID: &t.ID, Name: t.Name, LaunchType: launchType, LaunchedBy: &p.UserID, Config: cfg,
	})
	if err != nil {
		return nil, err
	}
	h.Log.InfoContext(ctx, "job launched", "job_id", job.ID, "job_template_id", t.ID, "by", p.Username)
	return apigen.LaunchJobTemplate201JSONResponse(toAPIJob(job)), nil
}

func toAPIJobTemplate(t store.JobTemplate) apigen.JobTemplate {
	overrides := make([]apigen.OverridableField, len(t.AllowOverrides))
	for i, o := range t.AllowOverrides {
		overrides[i] = apigen.OverridableField(o)
	}
	return apigen.JobTemplate{
		Id: t.ID, Name: t.Name, Description: t.Description, JobType: apigen.JobType(t.JobType),
		ExecutionEnvironmentId: t.ExecutionEnvironmentID, InventoryId: t.InventoryID,
		CredentialIds: nonNilSlice(t.CredentialIDs), Command: nonNilSlice(t.Command), ExtraVars: t.ExtraVars,
		Limit: t.Limit, TimeoutSeconds: t.TimeoutSeconds, AllowOverrides: overrides,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func toAPIJob(j store.Job) apigen.Job {
	c := j.Config
	out := apigen.Job{
		Id: j.ID, Name: j.Name, JobTemplateId: j.TemplateID, Status: apigen.JobStatus(j.Status),
		LaunchType: apigen.JobLaunchType(j.LaunchType), CancelRequested: j.CancelRequested,
		ExitCode: j.ExitCode, FailureReason: j.FailureReason,
		CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
		Config: apigen.JobConfig{
			JobType: apigen.JobType(c.JobType),
			ExecutionEnvironment: apigen.ExecutionEnvironmentRef{
				Id: c.ExecutionEnvironment.ID, Name: c.ExecutionEnvironment.Name,
				Image: c.ExecutionEnvironment.Image, PullPolicy: apigen.PullPolicy(c.ExecutionEnvironment.PullPolicy),
			},
			InventoryId: c.InventoryID, CredentialIds: nonNilSlice(c.CredentialIDs), Command: nonNilSlice(c.Command),
			ExtraVars: nonNilVars(c.ExtraVars), Limit: c.Limit, TimeoutSeconds: c.TimeoutSeconds,
		},
	}
	if j.LaunchedBy != nil {
		out.LaunchedBy = &apigen.UserRef{Id: j.LaunchedBy.ID, Username: j.LaunchedBy.Username}
	}
	return out
}

func nonNilVars(m map[string]any) apigen.Variables {
	if m == nil {
		return apigen.Variables{}
	}
	return m
}
