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
//
// Protocol 2 added projects and the ansible and script job types.
// Protocol 3 added the inventory_sync job type and its inventory report.
const (
	MinRunnerProtocol = 1
	MaxRunnerProtocol = 3
)

// minProtocolFor is the oldest runner protocol that can run each job type.
var minProtocolFor = map[string]int{"container": 1, "ansible": 2, "script": 2, "inventory_sync": 3}

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
	// Canceled while its pod was starting: end it here, before the spec (and
	// its decrypted credentials) goes out.
	if job.CancelRequested {
		if err := h.Store.EndJob(ctx, job.ID, store.JobCanceled, nil, "Canceled before it started", store.JobWaiting); err != nil && !errors.Is(err, store.ErrJobState) {
			return nil, err
		}
		return nil, conflict("the job was canceled before it started")
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
	if need := minProtocolFor[job.Config.JobType]; req.Params.Protocol < need {
		reason := fmt.Sprintf("The execution environment's runner (version %s, protocol %d) is too old to run %s jobs, "+
			"which need protocol %d. Rebuild the image with a newer runner.",
			req.Params.Version, req.Params.Protocol, job.Config.JobType, need)
		log.WarnContext(ctx, "runner too old for job type")
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
		JobId: job.ID, JobType: apigen.JobType(c.JobType), Command: nonNilSlice(c.Command),
		ExtraVars: nonNilVars(c.ExtraVars), Limit: c.Limit, TimeoutSeconds: c.TimeoutSeconds,
		Credentials: []apigen.RunnerCredential{}, Entrypoint: c.Entrypoint, Arguments: nonNilSlice(c.Arguments),
		Tags: c.Tags, SkipTags: c.SkipTags, Verbosity: c.Verbosity,
	}
	// The launcher's extra vars, so the runner can pass them untemplated.
	if o := job.Overrides; o != nil && len(o.ExtraVars) > 0 {
		v := apigen.Variables(o.ExtraVars)
		spec.OverrideVars = &v
	}
	if p := c.Project; p != nil {
		spec.Project = &apigen.RunnerProject{Url: p.ScmURL, Ref: p.ScmRef}
		if p.CredentialID != nil {
			cred, err := h.Store.GetCredential(ctx, *p.CredentialID)
			if errors.Is(err, store.ErrNotFound) {
				return spec, &specError{"the project's credential no longer exists"}
			}
			if err != nil {
				return spec, err
			}
			rc, err := h.runnerCredential(cred)
			if err != nil {
				return spec, err
			}
			spec.Project.Credential = &rc
		}
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
			rc, err := h.runnerCredential(cred)
			if err != nil {
				return spec, err
			}
			spec.Credentials = append(spec.Credentials, rc)
		}
	}
	return spec, nil
}

// runnerCredential decrypts a credential for a runner.
func (h *Handlers) runnerCredential(c store.Credential) (apigen.RunnerCredential, error) {
	inputs, err := h.Vault.Inputs(c)
	if err != nil {
		return apigen.RunnerCredential{}, &specError{err.Error()}
	}
	return apigen.RunnerCredential{Id: c.ID, Name: c.Name, Kind: apigen.CredentialKind(c.Kind), Inputs: inputs}, nil
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
	cancel, err := h.Store.AppendJobEvents(ctx, req.JobId, events, h.MaxJobOutput)
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
	if err := h.Store.FinishJob(ctx, req.JobId, status, b.ExitCode, deref(b.FailureReason), deref(b.ScmRevision)); err != nil {
		return nil, err
	}
	h.Log.InfoContext(ctx, "job finished", "job_id", req.JobId, "status", status, "exit_code", b.ExitCode,
		"scm_revision", deref(b.ScmRevision))
	return apigen.FinishRunnerJob204Response{}, nil
}
