package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/store"
)

func (h *Handlers) ListJobs(ctx context.Context, req apigen.ListJobsRequestObject) (apigen.ListJobsResponseObject, error) {
	p := req.Params
	items, total, err := h.Store.ListJobs(ctx,
		store.JobFilter{Status: string(deref(p.Status)), TemplateID: p.JobTemplateId}, pageOf(p.Limit, p.Offset))
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
	j, err := h.Store.GetJob(ctx, req.JobId)
	if err != nil {
		return nil, orNotFound(err, "Job")
	}
	return apigen.GetJob200JSONResponse(toAPIJob(j)), nil
}

func (h *Handlers) CancelJob(ctx context.Context, req apigen.CancelJobRequestObject) (apigen.CancelJobResponseObject, error) {
	j, err := h.Store.CancelJob(ctx, req.JobId)
	if errors.Is(err, store.ErrJobState) {
		return nil, conflict("the job has already finished")
	}
	if err != nil {
		return nil, orNotFound(err, "Job")
	}
	h.Log.InfoContext(ctx, "job cancel requested", "job_id", j.ID, "status", j.Status, "by", principalName(ctx))
	return apigen.CancelJob202JSONResponse(toAPIJob(j)), nil
}

func (h *Handlers) ListJobEvents(ctx context.Context, req apigen.ListJobEventsRequestObject) (apigen.ListJobEventsResponseObject, error) {
	limit := 1000
	setIf(&limit, req.Params.Limit)
	events, err := h.Store.ListJobEvents(ctx, req.JobId, deref(req.Params.AfterSeq), limit)
	if err != nil {
		return nil, orNotFound(err, "Job")
	}
	out := apigen.ListJobEvents200JSONResponse{Items: make([]apigen.JobEvent, 0, len(events))}
	for _, e := range events {
		out.Items = append(out.Items, toAPIJobEvent(e))
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
	// Check the job exists before committing to a 200 stream.
	if _, err := h.Store.GetJob(ctx, req.JobId); err != nil {
		return nil, orNotFound(err, "Job")
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
			events, err := s.h.Store.ListJobEvents(ctx, s.jobID, after, streamBatch)
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
