package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// NotificationTarget is a row in notification_targets. SecretValues is
// ciphertext; see the notify package.
type NotificationTarget struct {
	ID           uuid.UUID
	Name         string
	Description  string
	Kind         string
	Config       map[string]any
	SecretFields []string
	SecretValues []byte
	KeyID        string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const targetColumns = `n.id, n.name, n.description, n.kind, n.config, n.secret_fields, n.secret_values, n.key_id,
	n.created_at, n.updated_at`

func (n *NotificationTarget) scanTargets() []any {
	return []any{&n.ID, &n.Name, &n.Description, &n.Kind, &n.Config, &n.SecretFields, &n.SecretValues, &n.KeyID,
		&n.CreatedAt, &n.UpdatedAt}
}

// ListNotificationTargets returns notification targets, by name.
func (s *Store) ListNotificationTargets(ctx context.Context, search string, page Page) ([]NotificationTarget, int, error) {
	q := listQuery{
		columns: targetColumns, from: `notification_targets n`,
		where: `n.name ILIKE $1`, args: []any{likePattern(search)}, orderBy: `n.name`,
	}
	return list(ctx, s.pool, q, page, scanner[NotificationTarget]())
}

// GetNotificationTarget returns one notification target.
func (s *Store) GetNotificationTarget(ctx context.Context, id uuid.UUID) (NotificationTarget, error) {
	return getNotificationTarget(ctx, s.pool, id, false)
}

func getNotificationTarget(ctx context.Context, db querier, id uuid.UUID, forUpdate bool) (NotificationTarget, error) {
	sql := `SELECT ` + targetColumns + ` FROM notification_targets n WHERE n.id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	var n NotificationTarget
	err := db.QueryRow(ctx, sql, id).Scan(n.scanTargets()...)
	return n, mapErr(err)
}

// CreateNotificationTarget inserts n, whose ID and sealed secrets must be
// set.
func (s *Store) CreateNotificationTarget(ctx context.Context, n NotificationTarget) (NotificationTarget, error) {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO notification_targets (id, name, description, kind, config, secret_fields, secret_values, key_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		n.ID, n.Name, n.Description, n.Kind, nonNil(n.Config), nonNilStrings(n.SecretFields), n.SecretValues, n.KeyID)
	if err != nil {
		return NotificationTarget{}, mapErr(err)
	}
	return s.GetNotificationTarget(ctx, n.ID)
}

// UpdateNotificationTarget loads a target, lets update change it, and saves
// it. The kind can't change.
func (s *Store) UpdateNotificationTarget(ctx context.Context, id uuid.UUID, update func(*NotificationTarget) error) (NotificationTarget, error) {
	var out NotificationTarget
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		n, err := getNotificationTarget(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if err := update(&n); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE notification_targets SET name = $2, description = $3, config = $4, secret_fields = $5,
				secret_values = $6, key_id = $7
			WHERE id = $1`,
			id, n.Name, n.Description, nonNil(n.Config), nonNilStrings(n.SecretFields), n.SecretValues, n.KeyID); err != nil {
			return mapErr(err)
		}
		out, err = getNotificationTarget(ctx, tx, id, false)
		return err
	})
	return out, err
}

// DeleteNotificationTarget deletes a target, detaching it from templates
// and dropping its unsent notifications.
func (s *Store) DeleteNotificationTarget(ctx context.Context, id uuid.UUID) error {
	return s.deleteByID(ctx, "notification_targets", id)
}

// TemplateNotification is a target a template's jobs notify, and when.
type TemplateNotification struct {
	TargetID   uuid.UUID
	TargetName string
	TargetKind string
	Events     []string
}

// ListTemplateNotifications returns the targets a template notifies, by
// target name.
func (s *Store) ListTemplateNotifications(ctx context.Context, templateID uuid.UUID) ([]TemplateNotification, error) {
	return listTemplateNotifications(ctx, s.pool, templateID)
}

func listTemplateNotifications(ctx context.Context, db querier, templateID uuid.UUID) ([]TemplateNotification, error) {
	rows, err := db.Query(ctx, `
		SELECT n.id, n.name, n.kind, tn.events
		FROM job_template_notifications tn JOIN notification_targets n ON n.id = tn.target_id
		WHERE tn.job_template_id = $1 ORDER BY n.name`, templateID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (TemplateNotification, error) {
		var tn TemplateNotification
		err := row.Scan(&tn.TargetID, &tn.TargetName, &tn.TargetKind, &tn.Events)
		return tn, err
	})
}

// SetTemplateNotifications replaces the targets a template notifies and
// returns the new list. It returns ErrInvalidReference if a target does
// not exist, and ErrNotFound if the template doesn't.
func (s *Store) SetTemplateNotifications(ctx context.Context, templateID uuid.UUID, items []TemplateNotification) ([]TemplateNotification, error) {
	var out []TemplateNotification
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := getJobTemplate(ctx, tx, templateID, true); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM job_template_notifications WHERE job_template_id = $1`, templateID); err != nil {
			return err
		}
		for _, it := range items {
			if _, err := tx.Exec(ctx, `
				INSERT INTO job_template_notifications (job_template_id, target_id, events) VALUES ($1, $2, $3)`,
				templateID, it.TargetID, it.Events); err != nil {
				return mapErr(err)
			}
		}
		var err error
		out, err = listTemplateNotifications(ctx, tx, templateID)
		return err
	})
	return out, err
}

// Notification delivery statuses.
const (
	DeliveryPending = "pending"
	DeliverySent    = "sent"
	DeliveryFailed  = "failed"
)

// Delivery is a row in notification_deliveries: one notification about one
// job event to one target.
type Delivery struct {
	ID            int64
	JobID         uuid.UUID
	TargetID      uuid.UUID
	TargetName    string
	TargetKind    string
	Event         string
	Status        string
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
	CreatedAt     time.Time
	SentAt        *time.Time
}

const deliveryColumns = `d.id, d.job_id, d.target_id, n.name, n.kind, d.event, d.status, d.attempts,
	d.next_attempt_at, d.last_error, d.created_at, d.sent_at`

func (d *Delivery) scanTargets() []any {
	return []any{&d.ID, &d.JobID, &d.TargetID, &d.TargetName, &d.TargetKind, &d.Event, &d.Status, &d.Attempts,
		&d.NextAttemptAt, &d.LastError, &d.CreatedAt, &d.SentAt}
}

// ClaimDeliveries takes up to limit pending deliveries that are due, oldest
// first, counts an attempt for each, and holds them for lease: if the
// caller doesn't complete one in that time (e.g. it crashed), it becomes
// due again.
func (s *Store) ClaimDeliveries(ctx context.Context, limit int, lease time.Duration) ([]Delivery, error) {
	rows, err := s.pool.Query(ctx, `
		WITH claimed AS (
			UPDATE notification_deliveries SET attempts = attempts + 1,
				next_attempt_at = now() + make_interval(secs => $2)
			WHERE id IN (
				SELECT id FROM notification_deliveries
				WHERE status = 'pending' AND next_attempt_at <= now()
				ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED)
			RETURNING *)
		SELECT `+deliveryColumns+` FROM claimed d JOIN notification_targets n ON n.id = d.target_id
		ORDER BY d.id`, limit, lease.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanner[Delivery]())
}

// CompleteDelivery records the result of sending a claimed delivery: sent
// if sendErr is nil; otherwise pending again after retryAfter, or failed
// for good if retryAfter is 0.
func (s *Store) CompleteDelivery(ctx context.Context, id int64, sendErr error, retryAfter time.Duration) error {
	if sendErr == nil {
		_, err := s.pool.Exec(ctx, `
			UPDATE notification_deliveries SET status = 'sent', sent_at = now(), last_error = '' WHERE id = $1`, id)
		return err
	}
	msg := sendErr.Error()
	if len(msg) > 2000 {
		msg = msg[:2000] + "…"
	}
	if retryAfter > 0 {
		_, err := s.pool.Exec(ctx, `
			UPDATE notification_deliveries SET last_error = $2, next_attempt_at = now() + make_interval(secs => $3)
			WHERE id = $1`, id, msg, retryAfter.Seconds())
		return err
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE notification_deliveries SET status = 'failed', last_error = $2 WHERE id = $1`, id, msg)
	return err
}

// ListJobDeliveries returns the notifications about a job, oldest first.
func (s *Store) ListJobDeliveries(ctx context.Context, jobID uuid.UUID) ([]Delivery, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+deliveryColumns+` FROM notification_deliveries d JOIN notification_targets n ON n.id = d.target_id
		WHERE d.job_id = $1 ORDER BY d.id`, jobID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanner[Delivery]())
}

// CountPendingDeliveries returns how many notifications wait to be sent.
func (s *Store) CountPendingDeliveries(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries WHERE status = 'pending'`).Scan(&n)
	return n, err
}

// QueueStats are job queue numbers for metrics.
type QueueStats struct {
	// ByStatus counts pending, waiting and running jobs.
	ByStatus map[string]int
	// OldestPending is how long the oldest pending job has waited.
	OldestPending time.Duration
}

// GetQueueStats returns the current queue numbers.
func (s *Store) GetQueueStats(ctx context.Context) (QueueStats, error) {
	st := QueueStats{ByStatus: map[string]int{JobPending: 0, JobWaiting: 0, JobRunning: 0}}
	rows, err := s.pool.Query(ctx, `
		SELECT status, count(*) FROM jobs WHERE status IN ('pending', 'waiting', 'running') GROUP BY status`)
	if err != nil {
		return st, err
	}
	var status string
	var n int
	if _, err := pgx.ForEachRow(rows, []any{&status, &n}, func() error {
		st.ByStatus[status] = n
		return nil
	}); err != nil {
		return st, err
	}
	var secs float64
	err = s.pool.QueryRow(ctx, `
		SELECT coalesce(extract(epoch FROM now() - min(created_at)), 0)::float8 FROM jobs WHERE status = 'pending'`).Scan(&secs)
	st.OldestPending = time.Duration(secs * float64(time.Second))
	return st, err
}
