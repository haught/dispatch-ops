package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"dispatch-ops/internal/rbac"
)

// resourceTables maps resource types to their tables.
var resourceTables = map[rbac.ResourceType]string{
	rbac.Inventory:   "inventories",
	rbac.Credential:  "credentials",
	rbac.Project:     "projects",
	rbac.JobTemplate: "job_templates",
}

// RoleGrant is a row in role_grants, with the names of what it refers to.
type RoleGrant struct {
	ID           uuid.UUID
	Role         rbac.Role
	ResourceType *rbac.ResourceType // nil for a global role
	ResourceID   *uuid.UUID         // nil for a global role or one on every resource of the type
	ResourceName string
	UserID       *uuid.UUID
	Username     string
	TeamID       *uuid.UUID
	TeamName     string
	CreatedAt    time.Time
}

const grantColumns = `g.id, g.role, g.resource_type, g.resource_id,
	coalesce(CASE g.resource_type
		WHEN 'inventory' THEN (SELECT name FROM inventories WHERE id = g.resource_id)
		WHEN 'credential' THEN (SELECT name FROM credentials WHERE id = g.resource_id)
		WHEN 'project' THEN (SELECT name FROM projects WHERE id = g.resource_id)
		WHEN 'job_template' THEN (SELECT name FROM job_templates WHERE id = g.resource_id)
	END, ''),
	g.user_id, coalesce((SELECT username FROM users WHERE id = g.user_id), ''),
	g.team_id, coalesce((SELECT name FROM teams WHERE id = g.team_id), ''),
	g.created_at`

func (g *RoleGrant) scanTargets() []any {
	return []any{&g.ID, &g.Role, &g.ResourceType, &g.ResourceID, &g.ResourceName,
		&g.UserID, &g.Username, &g.TeamID, &g.TeamName, &g.CreatedAt}
}

// RoleGrantFilter narrows ListRoleGrants. Unset fields match everything.
type RoleGrantFilter struct {
	// ResourceType "global" matches global roles.
	ResourceType string
	ResourceID   *uuid.UUID
	UserID       *uuid.UUID
	TeamID       *uuid.UUID
	// HolderUserID matches grants to this user and to their teams.
	HolderUserID *uuid.UUID
}

// ListRoleGrants returns role grants: global roles first, then by resource
// type, resource name and role.
func (s *Store) ListRoleGrants(ctx context.Context, f RoleGrantFilter, page Page) ([]RoleGrant, int, error) {
	q := listQuery{columns: grantColumns, from: `role_grants g`}
	var conds []string
	arg := func(v any) string {
		q.args = append(q.args, v)
		return fmt.Sprintf("$%d", len(q.args))
	}
	switch f.ResourceType {
	case "":
	case "global":
		conds = append(conds, `g.resource_type IS NULL`)
	default:
		conds = append(conds, `g.resource_type = `+arg(f.ResourceType))
	}
	if f.ResourceID != nil {
		conds = append(conds, `g.resource_id = `+arg(*f.ResourceID))
	}
	if f.UserID != nil {
		conds = append(conds, `g.user_id = `+arg(*f.UserID))
	}
	if f.TeamID != nil {
		conds = append(conds, `g.team_id = `+arg(*f.TeamID))
	}
	if f.HolderUserID != nil {
		a := arg(*f.HolderUserID)
		conds = append(conds, `(g.user_id = `+a+` OR g.team_id IN (SELECT team_id FROM team_members WHERE user_id = `+a+`))`)
	}
	q.where = strings.Join(conds, " AND ")
	q.orderBy = `g.resource_type NULLS FIRST, 5, g.role, g.created_at, g.id`
	return list(ctx, s.pool, q, page, scanner[RoleGrant]())
}

// GetRoleGrant returns one role grant.
func (s *Store) GetRoleGrant(ctx context.Context, id uuid.UUID) (RoleGrant, error) {
	var g RoleGrant
	err := s.pool.QueryRow(ctx, `SELECT `+grantColumns+` FROM role_grants g WHERE g.id = $1`, id).Scan(g.scanTargets()...)
	return g, mapErr(err)
}

// NewRoleGrant holds the fields for granting a role. Exactly one of UserID
// and TeamID is set.
type NewRoleGrant struct {
	Role         rbac.Role
	ResourceType *rbac.ResourceType
	ResourceID   *uuid.UUID
	UserID       *uuid.UUID
	TeamID       *uuid.UUID
}

// CreateRoleGrant grants a role. It returns ErrConflict if the grant
// exists, and ErrInvalidReference if the user, team or resource does not.
func (s *Store) CreateRoleGrant(ctx context.Context, in NewRoleGrant) (RoleGrant, error) {
	var out RoleGrant
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if in.ResourceID != nil {
			table, ok := resourceTables[*in.ResourceType]
			if !ok {
				return ErrInvalidReference
			}
			// Lock the resource so it can't be deleted before the grant is
			// stored (its delete trigger would miss the grant).
			var found int
			err := tx.QueryRow(ctx, `SELECT 1 FROM `+table+` WHERE id = $1 FOR KEY SHARE`, *in.ResourceID).Scan(&found)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrInvalidReference
				}
				return err
			}
		}
		var id uuid.UUID
		err := tx.QueryRow(ctx, `
			INSERT INTO role_grants (role, resource_type, resource_id, user_id, team_id)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			in.Role, in.ResourceType, in.ResourceID, in.UserID, in.TeamID).Scan(&id)
		if err != nil {
			return mapErr(err)
		}
		return tx.QueryRow(ctx, `SELECT `+grantColumns+` FROM role_grants g WHERE g.id = $1`, id).Scan(out.scanTargets()...)
	})
	return out, err
}

// DeleteRoleGrant revokes a role grant.
func (s *Store) DeleteRoleGrant(ctx context.Context, id uuid.UUID) error {
	return s.deleteByID(ctx, "role_grants", id)
}

// ResourceRoles returns the roles a user holds, directly or through a team,
// on resources of type t: on the resource with the given ID and on every
// resource of the type. With a nil id, only the latter.
func (s *Store) ResourceRoles(ctx context.Context, userID uuid.UUID, t rbac.ResourceType, id *uuid.UUID) ([]rbac.Role, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT role FROM user_roles
		WHERE user_id = $1 AND resource_type = $2 AND (resource_id IS NULL OR resource_id = $3)`, userID, t, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[rbac.Role])
}

// readableBy restricts q to rows of resource type t that a user holds any
// role on (every role includes read). idExpr is the row's ID column.
func (q *listQuery) readableBy(userID *uuid.UUID, t rbac.ResourceType, idExpr string) {
	if userID == nil {
		return
	}
	q.args = append(q.args, *userID)
	q.and(fmt.Sprintf(`EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = $%d AND r.resource_type = '%s'
		AND (r.resource_id IS NULL OR r.resource_id = %s))`, len(q.args), t, idExpr))
}

func (q *listQuery) and(cond string) {
	if q.where == "" {
		q.where = cond
	} else {
		q.where = "(" + q.where + ") AND " + cond
	}
}
