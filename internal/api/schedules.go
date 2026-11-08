package api

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/cronspec"
	"dispatch-ops/internal/rbac"
	"dispatch-ops/internal/store"
)

func toAPISchedule(s store.Schedule) apigen.Schedule {
	out := apigen.Schedule{
		Id: s.ID, Name: s.Name, Description: s.Description, JobTemplateId: s.TemplateID, JobTemplateName: s.TemplateName,
		Cron: s.Cron, Timezone: s.Timezone, Enabled: s.Enabled, Overrides: toAPILaunchRequest(s.Overrides),
		NextRunAt: s.NextRunAt, LastRunAt: s.LastRunAt, LastJobId: s.LastJobID, LastError: s.LastError,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
	if s.CreatedBy != nil {
		out.CreatedBy = &apigen.UserRef{Id: s.CreatedBy.ID, Username: s.CreatedBy.Username}
	}
	return out
}

// parseCron validates a schedule's cron expression and time zone.
func parseCron(expr, tz string) (cronspec.Spec, error) {
	spec, err := cronspec.Parse(expr, tz)
	if err != nil {
		return spec, badRequest("%s", err.Error())
	}
	return spec, nil
}

// nextRun is when an enabled schedule next runs, or nil if it is disabled.
func nextRun(spec cronspec.Spec, enabled bool) *time.Time {
	if !enabled {
		return nil
	}
	n := spec.Next(time.Now())
	return &n
}

// getSchedule loads a schedule whose template the caller may read.
func (h *Handlers) getSchedule(ctx context.Context, id uuid.UUID) (store.Schedule, error) {
	sc, err := h.Store.GetSchedule(ctx, id)
	if err != nil {
		return sc, orNotFound(err, "Schedule")
	}
	if err := h.authorize(ctx, rbac.JobTemplate, sc.TemplateID, rbac.Read); err != nil {
		return sc, orScheduleNotFound(err)
	}
	return sc, nil
}

// authorizeScheduleChange checks that the caller may change or delete a
// schedule: admins of its template, and its creator while they may still
// launch the template.
func (h *Handlers) authorizeScheduleChange(ctx context.Context, sc store.Schedule) error {
	err := h.authorize(ctx, rbac.JobTemplate, sc.TemplateID, rbac.Admin)
	var ae *apiError
	if err == nil || !errors.As(err, &ae) || ae.status != 403 {
		return orScheduleNotFound(err)
	}
	if p := caller(ctx); sc.CreatedBy != nil && sc.CreatedBy.ID == p.UserID {
		if h.authorize(ctx, rbac.JobTemplate, sc.TemplateID, rbac.Execute) == nil {
			return nil
		}
	}
	return forbidden("Changing a schedule needs the admin role on its job template, or having created it")
}

func orScheduleNotFound(err error) error {
	var ae *apiError
	if errors.As(err, &ae) && ae.status == 404 {
		return notFound("Schedule")
	}
	return err
}

func (h *Handlers) ListSchedules(ctx context.Context, req apigen.ListSchedulesRequestObject) (apigen.ListSchedulesResponseObject, error) {
	p := req.Params
	items, total, err := h.Store.ListSchedules(ctx, store.ScheduleFilter{
		Search: deref(p.Search), TemplateID: p.JobTemplateId, ReadableBy: readableBy(ctx),
	}, pageOf(p.Limit, p.Offset))
	if err != nil {
		return nil, err
	}
	out := apigen.ListSchedules200JSONResponse{Items: make([]apigen.Schedule, 0, len(items)), Total: total}
	for _, s := range items {
		out.Items = append(out.Items, toAPISchedule(s))
	}
	return out, nil
}

func (h *Handlers) CreateSchedule(ctx context.Context, req apigen.CreateScheduleRequestObject) (apigen.CreateScheduleResponseObject, error) {
	b := req.Body
	if err := h.authorize(ctx, rbac.JobTemplate, b.JobTemplateId, rbac.Execute); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.status == 404 {
			return nil, badRequest("job template %s does not exist", b.JobTemplateId)
		}
		return nil, err
	}
	t, err := h.Store.GetJobTemplate(ctx, b.JobTemplateId)
	if errors.Is(err, store.ErrNotFound) {
		return nil, badRequest("job template %s does not exist", b.JobTemplateId)
	}
	if err != nil {
		return nil, err
	}
	f := store.ScheduleFields{
		Name: b.Name, Description: deref(b.Description), Cron: b.Cron, Timezone: "UTC", Enabled: true,
	}
	setIf(&f.Timezone, b.Timezone)
	setIf(&f.Enabled, b.Enabled)
	if b.Overrides != nil {
		f.Overrides = launchOverrides(*b.Overrides)
	}
	spec, err := parseCron(f.Cron, f.Timezone)
	if err != nil {
		return nil, err
	}
	if err := h.checkOverrideRefs(ctx, t, f.Overrides); err != nil {
		return nil, err
	}
	f.NextRunAt = nextRun(spec, f.Enabled)

	p := caller(ctx)
	sc, err := h.Store.CreateSchedule(ctx, t.ID, &p.UserID, f)
	if errors.Is(err, store.ErrConflict) {
		return nil, conflict("the job template already has a schedule named %q", f.Name)
	}
	if err != nil {
		return nil, err
	}
	out := toAPISchedule(sc)
	h.audit(ctx, auditRecord{
		Action: apigen.Create, ResourceType: apigen.AuditResourceTypeSchedule, ResourceID: sc.ID, ResourceName: sc.Name,
		After: out,
	})
	return apigen.CreateSchedule201JSONResponse(out), nil
}

func (h *Handlers) GetSchedule(ctx context.Context, req apigen.GetScheduleRequestObject) (apigen.GetScheduleResponseObject, error) {
	sc, err := h.getSchedule(ctx, req.ScheduleId)
	if err != nil {
		return nil, err
	}
	return apigen.GetSchedule200JSONResponse(toAPISchedule(sc)), nil
}

func (h *Handlers) UpdateSchedule(ctx context.Context, req apigen.UpdateScheduleRequestObject) (apigen.UpdateScheduleResponseObject, error) {
	cur, err := h.getSchedule(ctx, req.ScheduleId)
	if err != nil {
		return nil, err
	}
	if err := h.authorizeScheduleChange(ctx, cur); err != nil {
		return nil, err
	}
	t, err := h.Store.GetJobTemplate(ctx, cur.TemplateID)
	if err != nil {
		return nil, err
	}
	b := req.Body
	var before store.Schedule
	sc, err := h.Store.UpdateSchedule(ctx, req.ScheduleId, func(cur store.Schedule, f *store.ScheduleFields) error {
		before = cur
		setIf(&f.Name, b.Name)
		setIf(&f.Description, b.Description)
		setIf(&f.Cron, b.Cron)
		setIf(&f.Timezone, b.Timezone)
		setIf(&f.Enabled, b.Enabled)
		spec, err := parseCron(f.Cron, f.Timezone)
		if err != nil {
			return err
		}
		if b.Overrides != nil {
			f.Overrides = launchOverrides(*b.Overrides)
			if err := h.checkOverrideRefs(ctx, t, f.Overrides); err != nil {
				return err
			}
		}
		// Only a change of timing plans the next run afresh; otherwise a
		// run that is due right now would be skipped.
		if f.Cron != cur.Cron || f.Timezone != cur.Timezone || f.Enabled != cur.Enabled {
			f.NextRunAt = nextRun(spec, f.Enabled)
		}
		return nil
	})
	if errors.Is(err, store.ErrConflict) {
		return nil, conflict("the job template already has a schedule with that name")
	}
	if err != nil {
		return nil, orNotFound(err, "Schedule")
	}
	out := toAPISchedule(sc)
	h.audit(ctx, auditRecord{
		Action: apigen.Update, ResourceType: apigen.AuditResourceTypeSchedule, ResourceID: sc.ID, ResourceName: sc.Name,
		Before: toAPISchedule(before), After: out,
	})
	return apigen.UpdateSchedule200JSONResponse(out), nil
}

func (h *Handlers) DeleteSchedule(ctx context.Context, req apigen.DeleteScheduleRequestObject) (apigen.DeleteScheduleResponseObject, error) {
	sc, err := h.getSchedule(ctx, req.ScheduleId)
	if err != nil {
		return nil, err
	}
	if err := h.authorizeScheduleChange(ctx, sc); err != nil {
		return nil, err
	}
	if err := h.Store.DeleteSchedule(ctx, sc.ID); err != nil {
		return nil, orNotFound(err, "Schedule")
	}
	h.audit(ctx, auditRecord{
		Action: apigen.Delete, ResourceType: apigen.AuditResourceTypeSchedule, ResourceID: sc.ID, ResourceName: sc.Name,
		Before: toAPISchedule(sc),
	})
	return apigen.DeleteSchedule204Response{}, nil
}

func (h *Handlers) PreviewSchedule(ctx context.Context, req apigen.PreviewScheduleRequestObject) (apigen.PreviewScheduleResponseObject, error) {
	p := req.Params
	spec, err := parseCron(p.Cron, deref(p.Timezone))
	if err != nil {
		return nil, err
	}
	count := 5
	setIf(&count, p.Count)
	return apigen.PreviewSchedule200JSONResponse{Runs: spec.NextN(time.Now(), count)}, nil
}
