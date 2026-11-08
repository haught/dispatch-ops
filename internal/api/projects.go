package api

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/store"
)

// scmSchemes are the URL schemes a project may use. The runner allows git
// the same protocols (GIT_ALLOW_PROTOCOL), so remote helpers such as ext::
// can't run commands.
var scmSchemes = []string{"https", "http", "ssh", "git", "file"}

// scpLike matches git's scp-like syntax, [user@]host:path.
var scpLike = regexp.MustCompile(`^([A-Za-z0-9._~-]+@)?[A-Za-z0-9.-]+:[^:]`)

func checkScmURL(u string) error {
	if strings.Contains(u, "::") {
		return badRequest("scmUrl: git remote helpers (<transport>::<address>) are not allowed")
	}
	if scheme, _, ok := strings.Cut(u, "://"); ok {
		for _, s := range scmSchemes {
			if strings.EqualFold(scheme, s) {
				return nil
			}
		}
		return badRequest("scmUrl: the %q scheme is not supported; use %s, or scp-like git@host:path",
			scheme, strings.Join(scmSchemes, ", "))
	}
	if scpLike.MatchString(u) {
		return nil
	}
	return badRequest("scmUrl: %q is not a git URL; use %s://…, or scp-like git@host:path",
		u, strings.Join(scmSchemes, "://…, "))
}

// checkScmCredential verifies that id names an scm credential.
func (h *Handlers) checkScmCredential(ctx context.Context, id *uuid.UUID) error {
	if id == nil {
		return nil
	}
	c, err := h.Store.GetCredential(ctx, *id)
	if errors.Is(err, store.ErrNotFound) {
		return badRequest("credential %s does not exist", id)
	}
	if err != nil {
		return err
	}
	if c.Kind != "scm" {
		return badRequest("credential %q is a %s credential; projects need a source control (scm) credential", c.Name, c.Kind)
	}
	return nil
}

func (h *Handlers) ListProjects(ctx context.Context, req apigen.ListProjectsRequestObject) (apigen.ListProjectsResponseObject, error) {
	p := req.Params
	items, total, err := h.Store.ListProjects(ctx, deref(p.Search), pageOf(p.Limit, p.Offset))
	if err != nil {
		return nil, err
	}
	out := apigen.ListProjects200JSONResponse{Items: make([]apigen.Project, 0, len(items)), Total: total}
	for _, pr := range items {
		out.Items = append(out.Items, toAPIProject(pr))
	}
	return out, nil
}

func (h *Handlers) CreateProject(ctx context.Context, req apigen.CreateProjectRequestObject) (apigen.CreateProjectResponseObject, error) {
	b := req.Body
	if err := checkScmURL(b.ScmUrl); err != nil {
		return nil, err
	}
	if err := h.checkScmCredential(ctx, b.CredentialId); err != nil {
		return nil, err
	}
	ref := "main"
	setIf(&ref, b.ScmRef)
	p, err := h.Store.CreateProject(ctx, store.ProjectFields{
		Name: b.Name, Description: deref(b.Description), ScmURL: b.ScmUrl, ScmRef: ref, CredentialID: b.CredentialId,
	})
	if err != nil {
		return nil, err
	}
	h.Log.InfoContext(ctx, "project created", "project_id", p.ID, "by", principalName(ctx))
	return apigen.CreateProject201JSONResponse(toAPIProject(p)), nil
}

func (h *Handlers) GetProject(ctx context.Context, req apigen.GetProjectRequestObject) (apigen.GetProjectResponseObject, error) {
	p, err := h.Store.GetProject(ctx, req.ProjectId)
	if err != nil {
		return nil, orNotFound(err, "Project")
	}
	return apigen.GetProject200JSONResponse(toAPIProject(p)), nil
}

func (h *Handlers) UpdateProject(ctx context.Context, req apigen.UpdateProjectRequestObject) (apigen.UpdateProjectResponseObject, error) {
	b := req.Body
	if b.ScmUrl != nil {
		if err := checkScmURL(*b.ScmUrl); err != nil {
			return nil, err
		}
	}
	var cred *uuid.UUID
	if b.CredentialId.IsSpecified() && !b.CredentialId.IsNull() {
		id := b.CredentialId.MustGet()
		cred = &id
		if err := h.checkScmCredential(ctx, cred); err != nil {
			return nil, err
		}
	}
	p, err := h.Store.UpdateProject(ctx, req.ProjectId, func(f *store.ProjectFields) error {
		setIf(&f.Name, b.Name)
		setIf(&f.Description, b.Description)
		setIf(&f.ScmURL, b.ScmUrl)
		setIf(&f.ScmRef, b.ScmRef)
		if b.CredentialId.IsSpecified() {
			f.CredentialID = cred
		}
		return nil
	})
	if err != nil {
		return nil, orNotFound(err, "Project")
	}
	h.Log.InfoContext(ctx, "project updated", "project_id", p.ID, "by", principalName(ctx))
	return apigen.UpdateProject200JSONResponse(toAPIProject(p)), nil
}

func (h *Handlers) DeleteProject(ctx context.Context, req apigen.DeleteProjectRequestObject) (apigen.DeleteProjectResponseObject, error) {
	if err := h.Store.DeleteProject(ctx, req.ProjectId); err != nil {
		return nil, orNotFound(err, "Project")
	}
	h.Log.InfoContext(ctx, "project deleted", "project_id", req.ProjectId, "by", principalName(ctx))
	return apigen.DeleteProject204Response{}, nil
}

func toAPIProject(p store.Project) apigen.Project {
	return apigen.Project{
		Id: p.ID, Name: p.Name, Description: p.Description, ScmUrl: p.ScmURL, ScmRef: p.ScmRef,
		CredentialId: p.CredentialID, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}
