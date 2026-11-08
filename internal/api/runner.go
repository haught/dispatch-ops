package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/store"
)

// The runner protocol versions this server supports. Bump the maximum when
// the runner API changes compatibly; raise the minimum only when support for
// old execution environment images is dropped.
const (
	MinRunnerProtocol = 1
	MaxRunnerProtocol = 1
)

// runnerJob checks that the job token authenticates the job in the path.
func runnerJob(ctx context.Context, pathJobID uuid.UUID) error {
	id, ok := auth.JobFromContext(ctx)
	if !ok || id != pathJobID {
		return forbidden("the job token is for a different job")
	}
	return nil
}

// specError is a problem preparing a job that retrying won't fix.
type specError struct{ msg string }

func (e *specError) Error() string { return e.msg }

func (h *Handlers) GetRunnerJobSpec(ctx context.Context, req apigen.GetRunnerJobSpecRequestObject) (apigen.GetRunnerJobSpecResponseObject, error) {
	if err := runnerJob(ctx, req.JobId); err != nil {
		return nil, err
	}
	job, err := h.Store.GetJob(ctx, req.JobId)
	if err != nil {
		return nil, err
	}
	if job.Status != store.JobWaiting {
		return nil, conflict("the job specification can only be fetched once, before the job starts")
	}

	log := h.Log.With("job_id", job.ID, "runner_version", req.Params.Version, "runner_protocol", req.Params.Protocol)
	if p := req.Params.Protocol; p < MinRunnerProtocol || p > MaxRunnerProtocol {
		reason := fmt.Sprintf("The execution environment's runner (version %s, protocol %d) is not supported by this server, "+
			"which needs protocol %d to %d. Rebuild the image with a supported runner.",
			req.Params.Version, p, MinRunnerProtocol, MaxRunnerProtocol)
		log.WarnContext(ctx, "unsupported runner protocol")
		if err := h.Store.EndJob(ctx, job.ID, store.JobError, nil, reason, store.JobWaiting); err != nil && !errors.Is(err, store.ErrJobState) {
			return nil, err
		}
		return nil, badRequest("%s", reason)
	}

	spec, err := h.buildRunnerSpec(ctx, job)
	var se *specError
	if errors.As(err, &se) {
		reason := "Could not prepare the job: " + se.msg
		log.WarnContext(ctx, "job spec failed", "reason", se.msg)
		if err := h.Store.EndJob(ctx, job.ID, store.JobError, nil, reason, store.JobWaiting); err != nil && !errors.Is(err, store.ErrJobState) {
			return nil, err
		}
		return nil, conflict("%s", reason)
	}
	if err != nil {
		return nil, err
	}

	if _, err := h.Store.StartJob(ctx, job.ID, req.Params.Version); err != nil {
		if errors.Is(err, store.ErrJobState) {
			return nil, conflict("the job specification can only be fetched once, before the job starts")
		}
		return nil, err
	}
	log.InfoContext(ctx, "job started")
	return apigen.GetRunnerJobSpec200JSONResponse(spec), nil
}

func (h *Handlers) buildRunnerSpec(ctx context.Context, job store.Job) (apigen.RunnerJobSpec, error) {
	c := job.Config
	spec := apigen.RunnerJobSpec{
		JobId: job.ID, JobType: apigen.JobType(c.JobType), Command: c.Command, ExtraVars: nonNilVars(c.ExtraVars),
		Limit: c.Limit, TimeoutSeconds: c.TimeoutSeconds, Credentials: []apigen.RunnerCredential{},
	}
	if c.InventoryID != nil {
		snap, err := h.Store.GetInventorySnapshot(ctx, *c.InventoryID)
		if errors.Is(err, store.ErrNotFound) {
			return spec, &specError{"the job's inventory no longer exists"}
		}
		if err != nil {
			return spec, err
		}
		inv := apigen.InventoryExport(snap.Ansible())
		spec.Inventory = &inv
	}
	if len(c.CredentialIDs) > 0 {
		creds, err := h.Store.GetCredentials(ctx, c.CredentialIDs)
		if errors.Is(err, store.ErrNotFound) {
			return spec, &specError{"one of the job's credentials no longer exists"}
		}
		if err != nil {
			return spec, err
		}
		for _, cred := range creds {
			inputs, err := h.Vault.Inputs(cred)
			if err != nil {
				return spec, &specError{err.Error()}
			}
			spec.Credentials = append(spec.Credentials, apigen.RunnerCredential{
				Id: cred.ID, Name: cred.Name, Kind: apigen.CredentialKind(cred.Kind), Inputs: inputs,
			})
		}
	}
	return spec, nil
}

func (h *Handlers) PostRunnerJobEvents(ctx context.Context, req apigen.PostRunnerJobEventsRequestObject) (apigen.PostRunnerJobEventsResponseObject, error) {
	if err := runnerJob(ctx, req.JobId); err != nil {
		return nil, err
	}
	now := time.Now()
	events := make([]store.JobEvent, len(req.Body.Events))
	for i, e := range req.Body.Events {
		events[i] = store.JobEvent{Seq: e.Seq, Time: now, Type: e.Type, HostName: e.HostName, Stdout: e.Stdout}
		if e.Time != nil {
			events[i].Time = *e.Time
		}
		if e.Data != nil {
			events[i].Data = *e.Data
		}
	}
	cancel, err := h.Store.AppendJobEvents(ctx, req.JobId, events)
	if err != nil {
		return nil, err
	}
	return apigen.PostRunnerJobEvents200JSONResponse{CancelRequested: cancel}, nil
}

func (h *Handlers) RunnerHeartbeat(ctx context.Context, req apigen.RunnerHeartbeatRequestObject) (apigen.RunnerHeartbeatResponseObject, error) {
	if err := runnerJob(ctx, req.JobId); err != nil {
		return nil, err
	}
	cancel, err := h.Store.Heartbeat(ctx, req.JobId)
	if err != nil {
		return nil, err
	}
	return apigen.RunnerHeartbeat200JSONResponse{CancelRequested: cancel}, nil
}

func (h *Handlers) FinishRunnerJob(ctx context.Context, req apigen.FinishRunnerJobRequestObject) (apigen.FinishRunnerJobResponseObject, error) {
	if err := runnerJob(ctx, req.JobId); err != nil {
		return nil, err
	}
	b := req.Body
	status := string(b.Status)
	if err := h.Store.EndJob(ctx, req.JobId, status, b.ExitCode, deref(b.FailureReason), store.JobRunning); err != nil {
		return nil, err
	}
	h.Log.InfoContext(ctx, "job finished", "job_id", req.JobId, "status", status, "exit_code", b.ExitCode)
	return apigen.FinishRunnerJob204Response{}, nil
}
