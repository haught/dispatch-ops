package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"

	"dispatch-ops/internal/hostpattern"
)

// LaunchOverrides are the template fields a launch (or a schedule) replaces.
// Nil fields keep the template's value. They are stored as JSON on
// schedules.
type LaunchOverrides struct {
	InventoryID *uuid.UUID     `json:"inventoryId,omitempty"`
	ExtraVars   map[string]any `json:"extraVars,omitempty"`
	Limit       *string        `json:"limit,omitempty"`
	Command     []string       `json:"command,omitempty"`
	Tags        *string        `json:"tags,omitempty"`
	SkipTags    *string        `json:"skipTags,omitempty"`
}

// LaunchError is a reason a template can't be launched as asked, which the
// caller can fix (e.g. an override the template doesn't allow).
type LaunchError struct{ Msg string }

func (e *LaunchError) Error() string { return e.Msg }

// CheckAllowed returns a LaunchError if o overrides a field t doesn't
// allow to be overridden, or overrides one with a value no launcher may
// give (see CheckValues).
func (o LaunchOverrides) CheckAllowed(t JobTemplate) error {
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"inventory", o.InventoryID != nil},
		{"extraVars", o.ExtraVars != nil},
		{"limit", o.Limit != nil},
		{"command", o.Command != nil},
		{"tags", o.Tags != nil},
		{"skipTags", o.SkipTags != nil},
	} {
		if f.set && !slices.Contains(t.AllowOverrides, f.name) {
			return &LaunchError{fmt.Sprintf("the job template does not allow overriding %q at launch", f.name)}
		}
	}
	return o.CheckValues()
}

// CheckValues returns a LaunchError if o's values could reach the job's
// credentials rather than just steer the playbook:
//
//   - a limit that names a file (@path), which Ansible reads and echoes
//     into the job output;
//   - extra vars named ansible_*, which set how Ansible connects and
//     becomes (ansible_ssh_common_args can run a command, for one);
//   - extra vars with Jinja ({{, {% or {#) in a key or string value, which
//     Ansible evaluates as trusted templates, so lookup('file', …) or
//     pipe(…) could read the credential files.
//
// Extra vars the template itself sets aren't checked: changing those needs
// the use role on the template's credentials.
func (o LaunchOverrides) CheckValues() error {
	if o.Limit != nil {
		if err := hostpattern.CheckLimit(*o.Limit); err != nil {
			return &LaunchError{err.Error()}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(o.ExtraVars)) {
		if strings.HasPrefix(strings.ToLower(k), "ansible_") {
			return &LaunchError{fmt.Sprintf("extra var %q can't be overridden at launch: ansible_* variables control how Ansible connects to hosts", k)}
		}
		if path, bad := findJinja(k, o.ExtraVars[k]); bad {
			return &LaunchError{fmt.Sprintf("extra var %q can't be set to a Jinja template ({{, {%% or {#) at launch", path)}
		}
	}
	return nil
}

// hasJinja reports whether s contains the start of a Jinja expression,
// statement or comment.
func hasJinja(s string) bool {
	return strings.Contains(s, "{{") || strings.Contains(s, "{%") || strings.Contains(s, "{#")
}

// findJinja looks for Jinja in the key at path and in the strings of v,
// keys included, and returns where it is.
func findJinja(path string, v any) (string, bool) {
	if hasJinja(path) {
		return path, true
	}
	switch v := v.(type) {
	case string:
		return path, hasJinja(v)
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			if hasJinja(k) {
				return path + "." + k, true
			}
			if p, bad := findJinja(path+"."+k, v[k]); bad {
				return p, true
			}
		}
	case []any:
		for i, x := range v {
			if p, bad := findJinja(fmt.Sprintf("%s[%d]", path, i), x); bad {
				return p, true
			}
		}
	}
	return "", false
}

// ResolveJobConfig returns the configuration a job launched from t with o
// runs with. It returns a LaunchError if o isn't allowed or names an
// inventory that doesn't exist.
func (s *Store) ResolveJobConfig(ctx context.Context, t JobTemplate, o LaunchOverrides) (JobConfig, error) {
	return resolveJobConfig(ctx, s.pool, t, o)
}

func resolveJobConfig(ctx context.Context, db querier, t JobTemplate, o LaunchOverrides) (JobConfig, error) {
	if err := o.CheckAllowed(t); err != nil {
		return JobConfig{}, err
	}
	cfg := JobConfig{
		JobType: t.JobType, Entrypoint: t.Entrypoint, InventoryID: t.InventoryID,
		CredentialIDs: nonNilUUIDs(t.CredentialIDs), Command: t.Command, Arguments: t.Arguments,
		ExtraVars: maps.Clone(t.ExtraVars), Limit: t.Limit, Tags: t.Tags, SkipTags: t.SkipTags,
		Verbosity: t.Verbosity, TimeoutSeconds: t.TimeoutSeconds,
	}
	if cfg.ExtraVars == nil {
		cfg.ExtraVars = map[string]any{}
	}
	if o.InventoryID != nil {
		var exists bool
		if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM inventories WHERE id = $1)`, *o.InventoryID).Scan(&exists); err != nil {
			return JobConfig{}, err
		}
		if !exists {
			return JobConfig{}, &LaunchError{fmt.Sprintf("inventory %s does not exist", *o.InventoryID)}
		}
		cfg.InventoryID = o.InventoryID
	}
	maps.Copy(cfg.ExtraVars, o.ExtraVars)
	if o.Limit != nil {
		cfg.Limit = *o.Limit
	}
	// The template's own limit too, which may predate this check.
	if err := hostpattern.CheckLimit(cfg.Limit); err != nil {
		return JobConfig{}, &LaunchError{err.Error()}
	}
	if o.Command != nil {
		cfg.Command = o.Command
	}
	if o.Tags != nil {
		cfg.Tags = *o.Tags
	}
	if o.SkipTags != nil {
		cfg.SkipTags = *o.SkipTags
	}

	if t.ProjectID != nil {
		p, err := getProject(ctx, db, *t.ProjectID)
		if err != nil {
			return JobConfig{}, err
		}
		cfg.Project = &JobProject{ID: p.ID, Name: p.Name, ScmURL: p.ScmURL, ScmRef: p.ScmRef, CredentialID: p.CredentialID}
	}
	ee, err := getExecutionEnvironment(ctx, db, t.ExecutionEnvironmentID)
	if errors.Is(err, ErrNotFound) {
		return JobConfig{}, &LaunchError{"the job template's execution environment no longer exists"}
	}
	if err != nil {
		return JobConfig{}, err
	}
	cfg.ExecutionEnvironment = JobExecutionEnvironment{
		ID: ee.ID, Name: ee.Name, Image: ee.Image, PullPolicy: ee.PullPolicy, RegistryCredentialID: ee.RegistryCredentialID,
	}
	return cfg, nil
}

func nonNilUUIDs(ids []uuid.UUID) []uuid.UUID {
	if ids == nil {
		return []uuid.UUID{}
	}
	return ids
}
