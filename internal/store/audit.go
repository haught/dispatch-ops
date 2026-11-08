package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AuditEntry is a row in audit_log.
type AuditEntry struct {
	ID           int64
	Time         time.Time
	ActorID      *uuid.UUID
	ActorName    string
	AuthMethod   string
	Action       string
	ResourceType string
	ResourceID   *uuid.UUID
	ResourceName string
	Before       map[string]any
	After        map[string]any
}

const auditColumns = `a.id, a.created_at, a.actor_id, a.actor_name, a.auth_method, a.action,
	a.resource_type, a.resource_id, a.resource_name, a.before, a.after`

func (a *AuditEntry) scanTargets() []any {
	return []any{&a.ID, &a.Time, &a.ActorID, &a.ActorName, &a.AuthMethod, &a.Action,
		&a.ResourceType, &a.ResourceID, &a.ResourceName, &a.Before, &a.After}
}

// AddAuditEntry records an entry in the audit log. ID and Time are set by
// the database.
func (s *Store) AddAuditEntry(ctx context.Context, e AuditEntry) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO audit_log (actor_id, actor_name, auth_method, action, resource_type, resource_id, resource_name, before, after)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.ActorID, e.ActorName, e.AuthMethod, e.Action, e.ResourceType, e.ResourceID, e.ResourceName, e.Before, e.After)
	return err
}

// AuditFilter narrows ListAuditLog. Unset fields match everything.
type AuditFilter struct {
	ResourceType string
	ResourceID   *uuid.UUID
	ActorID      *uuid.UUID
	Action       string
	Since        *time.Time
	Until        *time.Time
}

// ListAuditLog returns audit log entries, newest first.
func (s *Store) ListAuditLog(ctx context.Context, f AuditFilter, page Page) ([]AuditEntry, int, error) {
	q := listQuery{columns: auditColumns, from: `audit_log a`, orderBy: `a.created_at DESC, a.id DESC`}
	var conds []string
	add := func(cond string, v any) {
		q.args = append(q.args, v)
		conds = append(conds, fmt.Sprintf(cond, len(q.args)))
	}
	if f.ResourceType != "" {
		add(`a.resource_type = $%d`, f.ResourceType)
	}
	if f.ResourceID != nil {
		add(`a.resource_id = $%d`, *f.ResourceID)
	}
	if f.ActorID != nil {
		add(`a.actor_id = $%d`, *f.ActorID)
	}
	if f.Action != "" {
		add(`a.action = $%d`, f.Action)
	}
	if f.Since != nil {
		add(`a.created_at >= $%d`, *f.Since)
	}
	if f.Until != nil {
		add(`a.created_at < $%d`, *f.Until)
	}
	q.where = strings.Join(conds, " AND ")
	return list(ctx, s.pool, q, page, scanner[AuditEntry]())
}
