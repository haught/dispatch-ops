package store

import (
	"context"
	"time"
)

// retentionBatch is how many rows one retention DELETE removes, so that
// deleting a large backlog doesn't hold long locks.
const retentionBatch = 500

// DeleteAuditEntriesBefore deletes audit log entries older than age and
// returns how many.
func (s *Store) DeleteAuditEntriesBefore(ctx context.Context, age time.Duration) (int64, error) {
	return deleteInBatches(ctx, s, `
		DELETE FROM audit_log WHERE id IN (
			SELECT id FROM audit_log WHERE created_at < now() - make_interval(secs => $1)
			ORDER BY id LIMIT $2)`, age.Seconds())
}

// deleteInBatches runs sql until it deletes fewer than retentionBatch rows,
// and returns how many it deleted. sql takes args, then the batch size.
func deleteInBatches(ctx context.Context, s *Store, sql string, args ...any) (int64, error) {
	args = append(args, retentionBatch)
	var total int64
	for {
		tag, err := s.pool.Exec(ctx, sql, args...)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < retentionBatch {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
