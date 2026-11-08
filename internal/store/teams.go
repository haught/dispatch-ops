package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Team is a row in teams, with its member count.
type Team struct {
	ID          uuid.UUID
	Name        string
	Description string
	OIDCGroups  []string
	MemberCount int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const teamColumns = `t.id, t.name, t.description, t.oidc_groups,
	(SELECT count(*) FROM team_members m WHERE m.team_id = t.id), t.created_at, t.updated_at`

func (t *Team) scanTargets() []any {
	return []any{&t.ID, &t.Name, &t.Description, &t.OIDCGroups, &t.MemberCount, &t.CreatedAt, &t.UpdatedAt}
}

// TeamFields are a team's editable fields.
type TeamFields struct {
	Name        string
	Description string
	OIDCGroups  []string
}

// ListTeams returns teams, by name. If userID is set, only that user's
// teams.
func (s *Store) ListTeams(ctx context.Context, search string, userID *uuid.UUID, page Page) ([]Team, int, error) {
	q := listQuery{
		columns: teamColumns, from: `teams t`,
		where: `t.name ILIKE $1`, args: []any{likePattern(search)}, orderBy: `t.name`,
	}
	if userID != nil {
		q.where += ` AND t.id IN (SELECT team_id FROM team_members WHERE user_id = $2)`
		q.args = append(q.args, *userID)
	}
	return list(ctx, s.pool, q, page, scanner[Team]())
}

// GetTeam returns one team.
func (s *Store) GetTeam(ctx context.Context, id uuid.UUID) (Team, error) {
	return getTeam(ctx, s.pool, id)
}

func getTeam(ctx context.Context, db querier, id uuid.UUID) (Team, error) {
	var t Team
	err := db.QueryRow(ctx, `SELECT `+teamColumns+` FROM teams t WHERE t.id = $1`, id).Scan(t.scanTargets()...)
	return t, mapErr(err)
}

// CreateTeam inserts a team.
func (s *Store) CreateTeam(ctx context.Context, in TeamFields) (Team, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO teams (name, description, oidc_groups) VALUES ($1, $2, $3) RETURNING id`,
		in.Name, in.Description, nonNilStrings(in.OIDCGroups)).Scan(&id)
	if err != nil {
		return Team{}, mapErr(err)
	}
	return s.GetTeam(ctx, id)
}

// UpdateTeam loads a team's fields, lets update change them, and saves the
// result. Members who joined through an OIDC group keep their membership
// until they next sign in.
func (s *Store) UpdateTeam(ctx context.Context, id uuid.UUID, update func(*TeamFields) error) (Team, error) {
	var out Team
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var f TeamFields
		err := tx.QueryRow(ctx, `SELECT name, description, oidc_groups FROM teams WHERE id = $1 FOR UPDATE`, id).
			Scan(&f.Name, &f.Description, &f.OIDCGroups)
		if err != nil {
			return mapErr(err)
		}
		if err := update(&f); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE teams SET name = $2, description = $3, oidc_groups = $4 WHERE id = $1`,
			id, f.Name, f.Description, nonNilStrings(f.OIDCGroups)); err != nil {
			return mapErr(err)
		}
		out, err = getTeam(ctx, tx, id)
		return err
	})
	return out, err
}

// DeleteTeam deletes a team, its memberships and the roles granted to it.
func (s *Store) DeleteTeam(ctx context.Context, id uuid.UUID) error {
	return s.deleteByID(ctx, "teams", id)
}

// TeamMember is a user's membership of a team.
type TeamMember struct {
	UserID      uuid.UUID
	Username    string
	DisplayName string
	// Source is manual (added through the API) or oidc (from the groups claim).
	Source  string
	AddedAt time.Time
}

// ListTeamMembers returns a team's members, by username. It returns
// ErrNotFound if the team does not exist.
func (s *Store) ListTeamMembers(ctx context.Context, teamID uuid.UUID) ([]TeamMember, error) {
	if _, err := s.GetTeam(ctx, teamID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.username, u.display_name, m.source, m.created_at
		FROM team_members m JOIN users u ON u.id = m.user_id
		WHERE m.team_id = $1 ORDER BY lower(u.username)`, teamID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (TeamMember, error) {
		var m TeamMember
		err := row.Scan(&m.UserID, &m.Username, &m.DisplayName, &m.Source, &m.AddedAt)
		return m, err
	})
}

// AddTeamMember adds a user to a team as a manual member, which OIDC sign-in
// won't remove. It returns ErrInvalidReference if the team or user does not
// exist.
func (s *Store) AddTeamMember(ctx context.Context, teamID, userID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO team_members (team_id, user_id, source) VALUES ($1, $2, 'manual')
		ON CONFLICT (team_id, user_id) DO UPDATE SET source = 'manual'`, teamID, userID)
	return mapErr(err)
}

// RemoveTeamMember removes a user from a team. It returns ErrNotFound if
// the user is not a member.
func (s *Store) RemoveTeamMember(ctx context.Context, teamID, userID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM team_members WHERE team_id = $1 AND user_id = $2`, teamID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
