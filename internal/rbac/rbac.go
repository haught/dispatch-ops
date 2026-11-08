// Package rbac defines roles and which actions they allow.
//
// A role is granted to a user or a team, either on one resource, on every
// resource of a type (resource ID omitted), or globally (no resource type:
// system_admin and auditor).
package rbac

import "slices"

// Role is a role name.
type Role string

const (
	// Admin manages a resource: edit, delete and grant roles on it.
	Admin Role = "admin"
	// Use lets a job template reference the resource.
	Use Role = "use"
	// Execute lets a job template be launched.
	Execute Role = "execute"
	// Read lets the resource be seen.
	Read Role = "read"

	// SystemAdmin can do anything.
	SystemAdmin Role = "system_admin"
	// Auditor can read everything, including the audit log.
	Auditor Role = "auditor"
)

// ResourceType is a kind of resource roles are granted on.
type ResourceType string

const (
	Inventory   ResourceType = "inventory"
	Credential  ResourceType = "credential"
	Project     ResourceType = "project"
	JobTemplate ResourceType = "job_template"
)

// ResourceTypes lists the resource types roles can be granted on.
var ResourceTypes = []ResourceType{Inventory, Credential, Project, JobTemplate}

// GlobalRoles are the roles granted without a resource type.
var GlobalRoles = []Role{SystemAdmin, Auditor}

// RolesFor returns the roles that can be granted on resources of type t.
func RolesFor(t ResourceType) []Role {
	switch t {
	case JobTemplate:
		return []Role{Admin, Execute, Read}
	case Inventory, Credential, Project:
		return []Role{Admin, Use, Read}
	}
	return nil
}

// Valid reports whether role can be granted on resource type t, or globally
// if t is empty.
func Valid(t ResourceType, role Role) bool {
	if t == "" {
		return slices.Contains(GlobalRoles, role)
	}
	return slices.Contains(RolesFor(t), role)
}

// implies lists the roles each role includes besides itself.
var implies = map[Role][]Role{
	Admin:   {Use, Execute, Read},
	Use:     {Read},
	Execute: {Read},
}

// Allows reports whether holding any of held grants want on a resource.
func Allows(held []Role, want Role) bool {
	for _, r := range held {
		if r == want || slices.Contains(implies[r], want) {
			return true
		}
	}
	return false
}
