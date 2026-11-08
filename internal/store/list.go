package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// listQuery describes a paged SELECT. where and its args are shared by the
// count query and the page query.
type listQuery struct {
	columns string
	from    string
	where   string
	args    []any
	orderBy string
}

// list runs q and returns one page of rows plus the total number of matching rows.
func list[T any](ctx context.Context, db querier, q listQuery, page Page, scan func(pgx.CollectableRow) (T, error)) ([]T, int, error) {
	where := q.where
	if where == "" {
		where = "true"
	}
	var total int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM `+q.from+` WHERE `+where, q.args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(q.args)
	sql := fmt.Sprintf(`SELECT %s FROM %s WHERE %s ORDER BY %s LIMIT $%d OFFSET $%d`,
		q.columns, q.from, where, q.orderBy, n+1, n+2)
	rows, err := db.Query(ctx, sql, append(q.args, page.Limit, page.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	items, err := pgx.CollectRows(rows, scan)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// scanner adapts a type's scanTargets method to pgx.CollectRows.
func scanner[T any, PT interface {
	*T
	scanTargets() []any
}]() func(pgx.CollectableRow) (T, error) {
	return func(row pgx.CollectableRow) (T, error) {
		var v T
		err := row.Scan(PT(&v).scanTargets()...)
		return v, err
	}
}

// nonNil returns m, or an empty map if m is nil, for NOT NULL jsonb columns.
func nonNil(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
