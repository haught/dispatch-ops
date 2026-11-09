package api

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/rbac"
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
	if scheme, rest, ok := strings.Cut(u, "://"); ok {
		for _, s := range scmSchemes {
			if strings.EqualFold(scheme, s) {
				if err := checkScmHost(rest); err != nil {
					return err
				}
				return checkScmUserinfo(u)
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

// checkScmHost refuses a URL whose host (in rest, the URL after "://") is
// not ASCII, or is percent-encoded non-ASCII. Such a host can look like
// another: Go lower-cases gİthub.com to github.com, so it would get
// github.com's origin and keep its credential, while git connects to
// xn--github-qyd.com. Internationalized names can be given in their xn--
// form.
func checkScmHost(rest string) error {
	authority := rest
	if i := strings.IndexAny(authority, "/?#"); i >= 0 {
		authority = authority[:i]
	}
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		authority = authority[i+1:]
	}
	if _, ok := asciiHost(authority); !ok {
		return badRequest("scmUrl: the host name must be ASCII; give an internationalized domain name in its xn-- form")
	}
	return nil
}

// asciiHost returns host percent-decoded (repeatedly, as git or a server
// might decode it) and lower-cased. ok is false if the result isn't
// printable ASCII, or doesn't decode.
func asciiHost(host string) (string, bool) {
	for i := 0; ; i++ {
		d, err := url.PathUnescape(host)
		if err != nil || i == 4 {
			return "", false
		}
		if d == host {
			break
		}
		host = d
	}
	for i := 0; i < len(host); i++ {
		if host[i] <= 0x20 || host[i] >= 0x7f {
			return "", false
		}
	}
	return strings.ToLower(host), true
}

// checkScmUserinfo refuses a secret in a repository URL, where everyone who
// can read the project (or a job that used it) would see it: a password in
// any URL, and for HTTP(S) any user info at all, since tokens are commonly
// given as the user name (https://<token>@host/...). A user name alone is
// fine for SSH (ssh://git@host/...). Secrets belong in an scm credential.
func checkScmUserinfo(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return nil
	}
	_, hasPassword := u.User.Password()
	httpish := strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")
	if hasPassword || (httpish && u.User.Username() != "") {
		return badRequest("scmUrl: don't put a user name, password or token in the URL, where everyone who can read the project sees it; " +
			"use an scm credential on the project instead")
	}
	return nil
}

// redactScmURL hides user info that may be a secret in a repository URL
// stored before checkScmUserinfo existed: a password, or any HTTP(S) user
// name. Saving the redacted URL back is refused, so the secret has to move
// to a credential.
func redactScmURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil || !strings.Contains(raw, "://") {
		return raw
	}
	_, hasPassword := u.User.Password()
	httpish := strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")
	switch {
	case httpish:
		u.User = url.User("REDACTED")
	case hasPassword:
		u.User = url.UserPassword(u.User.Username(), "REDACTED")
	default:
		return raw
	}
	return u.String()
}

// scmOrigin is where git sends a credential for the repository URL u: its
// scheme, host and port, like a web origin. scp-like URLs are ssh. A URL
// that doesn't parse, or whose host isn't ASCII (see checkScmHost), is its
// own origin, so any change to it counts.
func scmOrigin(u string) string {
	if !strings.Contains(u, "://") {
		if m := scpLike.FindStringSubmatch(u); m != nil {
			host, _, _ := strings.Cut(strings.TrimPrefix(u, m[1]), ":")
			return "ssh://" + strings.ToLower(host) + ":22"
		}
		return u
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
	host, ok := asciiHost(parsed.Hostname())
	if !ok {
		return u
	}
	scheme := strings.ToLower(parsed.Scheme)
	port := parsed.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80", "ssh": "22", "git": "9418"}[scheme]
	}
	return scheme + "://" + host + ":" + port
}

// checkScmCredential verifies that id names an scm credential the caller
// may use.
func (h *Handlers) checkScmCredential(ctx context.Context, id *uuid.UUID) error {
	if id == nil {
		return nil
	}
	if err := h.authorizeUse(ctx, rbac.Credential, *id); err != nil {
		return err
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
	items, total, err := h.Store.ListProjects(ctx, deref(p.Search), readableBy(ctx), pageOf(p.Limit, p.Offset))
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
	if err := h.authorizeCreate(ctx, rbac.Project); err != nil {
		return nil, err
	}
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
	if err := h.grantCreator(ctx, rbac.Project, p.ID); err != nil {
		return nil, err
	}
	out := toAPIProject(p)
	h.Log.InfoContext(ctx, "project created", "project_id", p.ID, "by", principalName(ctx))
	h.audit(ctx, auditRecord{
		Action: apigen.Create, ResourceType: apigen.AuditResourceTypeProject, ResourceID: p.ID, ResourceName: p.Name,
		After: out,
	})
	return apigen.CreateProject201JSONResponse(out), nil
}

func (h *Handlers) GetProject(ctx context.Context, req apigen.GetProjectRequestObject) (apigen.GetProjectResponseObject, error) {
	if err := h.authorize(ctx, rbac.Project, req.ProjectId, rbac.Read); err != nil {
		return nil, err
	}
	p, err := h.Store.GetProject(ctx, req.ProjectId)
	if err != nil {
		return nil, orNotFound(err, "Project")
	}
	return apigen.GetProject200JSONResponse(toAPIProject(p)), nil
}

func (h *Handlers) UpdateProject(ctx context.Context, req apigen.UpdateProjectRequestObject) (apigen.UpdateProjectResponseObject, error) {
	if err := h.authorize(ctx, rbac.Project, req.ProjectId, rbac.Admin); err != nil {
		return nil, err
	}
	before, err := h.Store.GetProject(ctx, req.ProjectId)
	if err != nil {
		return nil, orNotFound(err, "Project")
	}
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
		// Keeping a credential needs no use role; switching to one does.
		if before.CredentialID == nil || *before.CredentialID != id {
			if err := h.checkScmCredential(ctx, cred); err != nil {
				return nil, err
			}
		}
	}
	p, err := h.Store.UpdateProject(ctx, req.ProjectId, func(f *store.ProjectFields) error {
		oldURL, oldCred := f.ScmURL, f.CredentialID
		setIf(&f.Name, b.Name)
		setIf(&f.Description, b.Description)
		setIf(&f.ScmURL, b.ScmUrl)
		setIf(&f.ScmRef, b.ScmRef)
		if b.CredentialId.IsSpecified() {
			f.CredentialID = cred
		}
		// A kept credential needs the use role all the same when the
		// project moves to another server, which git would send it to.
		kept := f.CredentialID != nil && oldCred != nil && *f.CredentialID == *oldCred
		if kept && scmOrigin(oldURL) != scmOrigin(f.ScmURL) {
			if err := h.authorize(ctx, rbac.Credential, *f.CredentialID, rbac.Use); err != nil {
				if ae := (*apiError)(nil); !errors.As(err, &ae) {
					return err
				}
				return forbidden("Moving the project to another server (scheme, host or port) needs the use role on its credential, " +
					"which git would send there; ask the credential's admin, or remove or replace the credential")
			}
		}
		return nil
	})
	if err != nil {
		return nil, orNotFound(err, "Project")
	}
	out := toAPIProject(p)
	h.Log.InfoContext(ctx, "project updated", "project_id", p.ID, "by", principalName(ctx))
	h.audit(ctx, auditRecord{
		Action: apigen.Update, ResourceType: apigen.AuditResourceTypeProject, ResourceID: p.ID, ResourceName: p.Name,
		Before: toAPIProject(before), After: out,
	})
	return apigen.UpdateProject200JSONResponse(out), nil
}

func (h *Handlers) DeleteProject(ctx context.Context, req apigen.DeleteProjectRequestObject) (apigen.DeleteProjectResponseObject, error) {
	if err := h.authorize(ctx, rbac.Project, req.ProjectId, rbac.Admin); err != nil {
		return nil, err
	}
	p, err := h.Store.GetProject(ctx, req.ProjectId)
	if err != nil {
		return nil, orNotFound(err, "Project")
	}
	if err := h.Store.DeleteProject(ctx, req.ProjectId); err != nil {
		return nil, orNotFound(err, "Project")
	}
	h.Log.InfoContext(ctx, "project deleted", "project_id", req.ProjectId, "by", principalName(ctx))
	h.audit(ctx, auditRecord{
		Action: apigen.Delete, ResourceType: apigen.AuditResourceTypeProject, ResourceID: p.ID, ResourceName: p.Name,
		Before: toAPIProject(p),
	})
	return apigen.DeleteProject204Response{}, nil
}

func toAPIProject(p store.Project) apigen.Project {
	return apigen.Project{
		Id: p.ID, Name: p.Name, Description: p.Description, ScmUrl: redactScmURL(p.ScmURL), ScmRef: p.ScmRef,
		CredentialId: p.CredentialID, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}
