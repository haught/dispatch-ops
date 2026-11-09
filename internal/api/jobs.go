package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/metrics"
	"dispatch-ops/internal/rbac"
	"dispatch-ops/internal/store"
)

func (h *Handlers) ListJobs(ctx context.Context, req apigen.ListJobsRequestObject) (apigen.ListJobsResponseObject, error) {
	p := req.Params
	items, total, err := h.Store.ListJobs(ctx,
		store.JobFilter{Status: string(deref(p.Status)), TemplateID: p.JobTemplateId, ScheduleID: p.ScheduleId,
			SyncInventoryID: p.SyncInventoryId, ReadableBy: readableBy(ctx)},
		pageOf(p.Limit, p.Offset))
	if err != nil {
		return nil, err
	}
	out := apigen.ListJobs200JSONResponse{Items: make([]apigen.Job, 0, len(items)), Total: total}
	for _, j := range items {
		out.Items = append(out.Items, toAPIJob(j))
	}
	return out, nil
}

func (h *Handlers) GetJob(ctx context.Context, req apigen.GetJobRequestObject) (apigen.GetJobResponseObject, error) {
	j, err := h.getJob(ctx, req.JobId)
	if err != nil {
		return nil, err
	}
	return apigen.GetJob200JSONResponse(toAPIJob(j)), nil
}

func (h *Handlers) CancelJob(ctx context.Context, req apigen.CancelJobRequestObject) (apigen.CancelJobResponseObject, error) {
	j, err := h.Store.GetJob(ctx, req.JobId)
	if err != nil {
		return nil, orNotFound(err, "Job")
	}
	if err := h.authorizeJob(ctx, j, rbac.Execute); err != nil {
		return nil, err
	}
	j, err = h.Store.CancelJob(ctx, req.JobId)
	if errors.Is(err, store.ErrJobState) {
		return nil, conflict("the job has already finished")
	}
	if err != nil {
		return nil, orNotFound(err, "Job")
	}
	h.Log.InfoContext(ctx, "job cancel requested", "job_id", j.ID, "status", j.Status, "by", principalName(ctx))
	h.audit(ctx, auditRecord{
		Action: apigen.Cancel, ResourceType: apigen.AuditResourceTypeJob, ResourceID: j.ID, ResourceName: j.Name,
		After: map[string]any{"status": j.Status},
	})
	return apigen.CancelJob202JSONResponse(toAPIJob(j)), nil
}

func (h *Handlers) ListJobEvents(ctx context.Context, req apigen.ListJobEventsRequestObject) (apigen.ListJobEventsResponseObject, error) {
	if _, err := h.getJob(ctx, req.JobId); err != nil {
		return nil, err
	}
	limit := 1000
	setIf(&limit, req.Params.Limit)
	events, err := h.Store.ListJobEvents(ctx, req.JobId,
		store.JobEventFilter{AfterSeq: deref(req.Params.AfterSeq), HostName: deref(req.Params.HostName)}, limit)
	if err != nil {
		return nil, orNotFound(err, "Job")
	}
	out := apigen.ListJobEvents200JSONResponse{Items: make([]apigen.JobEvent, 0, len(events))}
	for _, e := range events {
		out.Items = append(out.Items, toAPIJobEvent(e))
	}
	return out, nil
}

func (h *Handlers) ListJobHosts(ctx context.Context, req apigen.ListJobHostsRequestObject) (apigen.ListJobHostsResponseObject, error) {
	if _, err := h.getJob(ctx, req.JobId); err != nil {
		return nil, err
	}
	hosts, err := h.Store.ListJobHostSummaries(ctx, req.JobId)
	if err != nil {
		return nil, orNotFound(err, "Job")
	}
	out := apigen.ListJobHosts200JSONResponse{Items: make([]apigen.JobHostSummary, 0, len(hosts))}
	for _, s := range hosts {
		out.Items = append(out.Items, apigen.JobHostSummary{
			HostName: s.HostName, Status: apigen.JobHostSummaryStatus(s.Status()), Ok: s.OK, Changed: s.Changed,
			Failed: s.Failed, Unreachable: s.Unreachable, Skipped: s.Skipped, Rescued: s.Rescued, Ignored: s.Ignored,
		})
	}
	return out, nil
}

func toAPIJobEvent(e store.JobEvent) apigen.JobEvent {
	out := apigen.JobEvent{Seq: e.Seq, Time: e.Time, Type: e.Type, Stdout: e.Stdout, HostName: e.HostName}
	if e.Data != nil {
		d := e.Data
		out.Data = &d
	}
	return out
}

// --- Live stream (server-sent events) ---

// streamKeepalive is how often an idle stream sends a comment, so proxies
// don't time it out. Each keepalive also re-checks the job, in case a
// notification was lost.
const streamKeepalive = 15 * time.Second

// streamBatch is how many events are read from the database at a time.
const streamBatch = 500

func (h *Handlers) StreamJob(ctx context.Context, req apigen.StreamJobRequestObject) (apigen.StreamJobResponseObject, error) {
	after := deref(req.Params.AfterSeq)
	if id := deref(req.Params.LastEventID); id != "" {
		seq, err := strconv.ParseInt(id, 10, 64)
		if err != nil || seq < 0 {
			return nil, badRequest("Last-Event-ID must be an event sequence number")
		}
		after = seq
	}
	// Check the job exists, and may be seen, before committing to a 200 stream.
	if _, err := h.getJob(ctx, req.JobId); err != nil {
		return nil, err
	}
	return jobStream{h: h, ctx: ctx, jobID: req.JobId, after: after}, nil
}

// jobStream writes a job's events and status changes as server-sent events.
// It implements the generated response interface, so it is written by the
// strict handler like any other response.
type jobStream struct {
	h     *Handlers
	ctx   context.Context
	jobID uuid.UUID
	after int64
}

func (s jobStream) VisitStreamJobResponse(w http.ResponseWriter) error {
	ctx := s.ctx
	wake, unsubscribe := s.h.Hub.Subscribe(s.jobID)
	defer unsubscribe()
	metrics.JobStreams.Inc()
	defer metrics.JobStreams.Dec()

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("X-Accel-Buffering", "no") // ask nginx-style proxies not to buffer
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	keepalive := time.NewTicker(streamKeepalive)
	defer keepalive.Stop()

	after := s.after
	lastStatus := ""
	lastCancel := false
	for {
		// Read the job before its events: a runner sends all events before
		// it reports the final status, so if the job had finished by now,
		// the events read next are complete.
		job, err := s.h.Store.GetJob(ctx, s.jobID)
		if err != nil {
			return s.end(err)
		}
		if job.Status != lastStatus || job.CancelRequested != lastCancel {
			lastStatus, lastCancel = job.Status, job.CancelRequested
			if err := writeSSE(w, "status", "", toAPIJob(job)); err != nil {
				return s.end(err)
			}
		}
		for {
			events, err := s.h.Store.ListJobEvents(ctx, s.jobID, store.JobEventFilter{AfterSeq: after}, streamBatch)
			if err != nil {
				return s.end(err)
			}
			for _, e := range events {
				if err := writeSSE(w, "job_event", strconv.FormatInt(e.Seq, 10), toAPIJobEvent(e)); err != nil {
					return s.end(err)
				}
				after = e.Seq
			}
			if len(events) < streamBatch {
				break
			}
		}
		if store.JobFinished(job.Status) {
			if err := writeSSE(w, "end", "", map[string]string{"status": job.Status}); err != nil {
				return s.end(err)
			}
			_ = rc.Flush()
			return nil
		}
		if err := rc.Flush(); err != nil {
			return s.end(err)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-wake:
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return s.end(err)
			}
		}
	}
}

// end finishes a stream after an error. Once the 200 header is sent no
// problem response can follow, so errors are only logged, and a client that
// went away is not an error at all.
func (s jobStream) end(err error) error {
	if s.ctx.Err() == nil {
		s.h.Log.WarnContext(s.ctx, "job stream ended", "job_id", s.jobID, "err", err)
	}
	return nil
}

func writeSSE(w http.ResponseWriter, event, id string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if id != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}

// --- Relaunch ---

func (h *Handlers) RelaunchJob(ctx context.Context, req apigen.RelaunchJobRequestObject) (apigen.RelaunchJobResponseObject, error) {
	j, err := h.getJob(ctx, req.JobId)
	if err != nil {
		return nil, err
	}
	p := caller(ctx)
	if j.Config.JobType == store.JobTypeInventorySync {
		return h.relaunchSync(ctx, j, req.Body)
	}
	if err := h.authorizeRelaunch(ctx, j); err != nil {
		return nil, err
	}

	cfg := j.Config
	hosts := apigen.RelaunchRequestHostsAll
	if req.Body != nil && req.Body.Hosts != nil {
		hosts = *req.Body.Hosts
	}
	if hosts == apigen.RelaunchRequestHostsFailed {
		limit, err := h.failedHostsLimit(ctx, j)
		if err != nil {
			return nil, err
		}
		cfg.Limit = limit
	}

	job, err := h.Store.CreateJob(ctx, store.NewJob{
		TemplateID: j.TemplateID, Name: j.Name, LaunchType: "relaunch", LaunchedBy: &p.UserID, Config: cfg, RelaunchOf: &j.ID,
	})
	if err != nil {
		return nil, err
	}
	h.Log.InfoContext(ctx, "job relaunched", "job_id", job.ID, "relaunch_of", j.ID, "hosts", hosts, "by", p.Username)
	h.audit(ctx, auditRecord{
		Action: apigen.Launch, ResourceType: apigen.AuditResourceTypeJob, ResourceID: job.ID, ResourceName: job.Name,
		After: map[string]any{"jobTemplateId": j.TemplateID, "launchType": "relaunch", "relaunchOf": j.ID, "hosts": hosts},
	})
	return apigen.RelaunchJob201JSONResponse(toAPIJob(job)), nil
}

// relaunchSync syncs the inventory a sync job synced again, from its
// source as it is now. That needs the admin role on the inventory.
func (h *Handlers) relaunchSync(ctx context.Context, j store.Job, body *apigen.RelaunchJobJSONRequestBody) (apigen.RelaunchJobResponseObject, error) {
	if body != nil && body.Hosts != nil && *body.Hosts == apigen.RelaunchRequestHostsFailed {
		return nil, badRequest("only Ansible jobs can be relaunched on their failed hosts")
	}
	if j.SyncInventoryID == nil {
		return nil, badRequest("the inventory this job synced no longer exists")
	}
	if err := h.authorize(ctx, rbac.Inventory, *j.SyncInventoryID, rbac.Admin); err != nil {
		return nil, forbidden("Syncing an inventory again needs the admin role on it")
	}
	p := caller(ctx)
	job, err := h.launchSync(ctx, *j.SyncInventoryID, store.NewSync{LaunchType: "relaunch", LaunchedBy: &p.UserID, RelaunchOf: &j.ID})
	if err != nil {
		return nil, err
	}
	return apigen.RelaunchJob201JSONResponse(toAPIJob(job)), nil
}

// authorizeRelaunch checks that the caller may run a job again: they need
// execute on its template, and use on whatever the job used that the
// template no longer references, since the job reruns with its own
// configuration rather than the template's current one.
func (h *Handlers) authorizeRelaunch(ctx context.Context, j store.Job) error {
	p := caller(ctx)
	if p.IsAdmin {
		return nil
	}
	if j.TemplateID == nil {
		return forbidden("The job's template was deleted; only system administrators can relaunch it")
	}
	if err := h.authorize(ctx, rbac.JobTemplate, *j.TemplateID, rbac.Execute); err != nil {
		return forbidden("Relaunching needs the execute role on the job's template")
	}
	t, err := h.Store.GetJobTemplate(ctx, *j.TemplateID)
	if err != nil {
		return err
	}
	c := j.Config
	if c.InventoryID != nil && (t.InventoryID == nil || *t.InventoryID != *c.InventoryID) {
		if err := h.authorize(ctx, rbac.Inventory, *c.InventoryID, rbac.Use); err != nil {
			return forbidden("The job ran against an inventory its template no longer uses; relaunching it needs the use role on that inventory")
		}
	}
	if c.Project != nil && (t.ProjectID == nil || *t.ProjectID != c.Project.ID) {
		if err := h.authorize(ctx, rbac.Project, c.Project.ID, rbac.Use); err != nil {
			return forbidden("The job ran from a project its template no longer uses; relaunching it needs the use role on that project")
		}
	}
	for _, id := range c.CredentialIDs {
		if !slices.Contains(t.CredentialIDs, id) {
			if err := h.authorize(ctx, rbac.Credential, id, rbac.Use); err != nil {
				return forbidden("The job used a credential its template no longer uses; relaunching it needs the use role on that credential")
			}
		}
	}
	return nil
}

// failedHostsLimit is the limit pattern selecting the hosts that failed or
// were unreachable in a finished Ansible job.
func (h *Handlers) failedHostsLimit(ctx context.Context, j store.Job) (string, error) {
	if j.Config.JobType != string(apigen.Ansible) {
		return "", badRequest("only Ansible jobs can be relaunched on their failed hosts")
	}
	if !store.JobFinished(j.Status) {
		return "", conflict("the job has not finished, so it is not known which hosts failed")
	}
	summaries, err := h.Store.ListJobHostSummaries(ctx, j.ID)
	if err != nil {
		return "", err
	}
	var failed []string
	for _, s := range summaries {
		if s.Failed > 0 || s.Unreachable > 0 {
			failed = append(failed, s.HostName)
		}
	}
	if len(failed) == 0 {
		return "", badRequest("no hosts failed or were unreachable in this job")
	}
	return strings.Join(failed, ","), nil
}

// --- Notifications ---

func (h *Handlers) ListJobNotifications(ctx context.Context, req apigen.ListJobNotificationsRequestObject) (apigen.ListJobNotificationsResponseObject, error) {
	if _, err := h.getJob(ctx, req.JobId); err != nil {
		return nil, err
	}
	ds, err := h.Store.ListJobDeliveries(ctx, req.JobId)
	if err != nil {
		return nil, err
	}
	out := apigen.ListJobNotifications200JSONResponse{Items: make([]apigen.NotificationDelivery, 0, len(ds))}
	for _, d := range ds {
		item := apigen.NotificationDelivery{
			Id: d.ID, TargetId: d.TargetID, TargetName: d.TargetName, TargetKind: apigen.NotificationKind(d.TargetKind),
			Event: apigen.NotificationEvent(d.Event), Status: apigen.NotificationDeliveryStatus(d.Status),
			Attempts: d.Attempts, LastError: d.LastError, CreatedAt: d.CreatedAt, SentAt: d.SentAt,
		}
		if d.Status == store.DeliveryPending {
			item.NextAttemptAt = &d.NextAttemptAt
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}
