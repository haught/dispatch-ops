package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"dispatch-ops/internal/hostpattern"
)

// JobTypeInventorySync is the job type of inventory syncs. Job templates
// can't have it.
const JobTypeInventorySync = "inventory_sync"

// ErrSyncActive means the inventory already has a sync that hasn't finished.
var ErrSyncActive = errors.New("a sync of this inventory is already pending or running")

// InventorySource is a row in inventory_sources, with its credentials.
type InventorySource struct {
	InventoryID              uuid.UUID
	ProjectID                uuid.UUID
	ProjectName              string
	SourcePath               string
	ExecutionEnvironmentID   uuid.UUID
	ExecutionEnvironmentName string
	CredentialIDs            []uuid.UUID
	TimeoutSeconds           int
	Verbosity                int
	LastJobID                *uuid.UUID
	LastJobStatus            *string
	LastSyncedAt             *time.Time
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

// InventorySourceFields are a source's editable fields.
type InventorySourceFields struct {
	ProjectID              uuid.UUID
	SourcePath             string
	ExecutionEnvironmentID uuid.UUID
	CredentialIDs          []uuid.UUID
	TimeoutSeconds         int
	Verbosity              int
}

const inventorySourceColumns = `s.inventory_id, s.project_id, p.name, s.source_path,
	s.execution_environment_id, ee.name,
	ARRAY(SELECT c.credential_id FROM inventory_source_credentials c WHERE c.inventory_id = s.inventory_id ORDER BY c.credential_id),
	s.timeout_seconds, s.verbosity, s.last_job_id, lj.status, s.last_synced_at, s.created_at, s.updated_at`

const inventorySourceFrom = `inventory_sources s
	JOIN projects p ON p.id = s.project_id
	JOIN execution_environments ee ON ee.id = s.execution_environment_id
	LEFT JOIN jobs lj ON lj.id = s.last_job_id`

// GetInventorySource returns an inventory's source, or ErrNotFound if it
// has none.
func (s *Store) GetInventorySource(ctx context.Context, inventoryID uuid.UUID) (InventorySource, error) {
	return getInventorySource(ctx, s.pool, inventoryID, false)
}

func getInventorySource(ctx context.Context, db querier, inventoryID uuid.UUID, forUpdate bool) (InventorySource, error) {
	sql := `SELECT ` + inventorySourceColumns + ` FROM ` + inventorySourceFrom + ` WHERE s.inventory_id = $1`
	if forUpdate {
		sql += ` FOR UPDATE OF s`
	}
	var src InventorySource
	err := db.QueryRow(ctx, sql, inventoryID).Scan(&src.InventoryID, &src.ProjectID, &src.ProjectName, &src.SourcePath,
		&src.ExecutionEnvironmentID, &src.ExecutionEnvironmentName, &src.CredentialIDs,
		&src.TimeoutSeconds, &src.Verbosity, &src.LastJobID, &src.LastJobStatus, &src.LastSyncedAt,
		&src.CreatedAt, &src.UpdatedAt)
	return src, mapErr(err)
}

// InventoryHasSource reports whether an inventory is synced from a source.
func (s *Store) InventoryHasSource(ctx context.Context, inventoryID uuid.UUID) (bool, error) {
	var has bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM inventory_sources WHERE inventory_id = $1)`, inventoryID).Scan(&has)
	return has, err
}

// SetInventorySource gives an inventory a source, or replaces its source's
// fields. It returns ErrInvalidReference if the inventory, project,
// execution environment or a credential doesn't exist.
func (s *Store) SetInventorySource(ctx context.Context, inventoryID uuid.UUID, f InventorySourceFields) (InventorySource, error) {
	var out InventorySource
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO inventory_sources (inventory_id, project_id, source_path, execution_environment_id, timeout_seconds, verbosity)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (inventory_id) DO UPDATE SET project_id = EXCLUDED.project_id, source_path = EXCLUDED.source_path,
				execution_environment_id = EXCLUDED.execution_environment_id,
				timeout_seconds = EXCLUDED.timeout_seconds, verbosity = EXCLUDED.verbosity`,
			inventoryID, f.ProjectID, f.SourcePath, f.ExecutionEnvironmentID, f.TimeoutSeconds, f.Verbosity)
		if err != nil {
			return mapErr(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM inventory_source_credentials WHERE inventory_id = $1`, inventoryID); err != nil {
			return err
		}
		if len(f.CredentialIDs) > 0 {
			if _, err := tx.Exec(ctx, `
				INSERT INTO inventory_source_credentials (inventory_id, credential_id)
				SELECT $1, c FROM unnest($2::uuid[]) AS c ON CONFLICT DO NOTHING`,
				inventoryID, f.CredentialIDs); err != nil {
				return mapErr(err)
			}
		}
		out, err = getInventorySource(ctx, tx, inventoryID, false)
		return err
	})
	return out, err
}

// DeleteInventorySource removes an inventory's source. The hosts and groups
// it synced stay, and can be edited again.
func (s *Store) DeleteInventorySource(ctx context.Context, inventoryID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM inventory_sources WHERE inventory_id = $1`, inventoryID)
	if err != nil {
		return mapDeleteErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// NewSync holds the fields for launching an inventory sync.
type NewSync struct {
	LaunchType string
	LaunchedBy *uuid.UUID
	ScheduleID *uuid.UUID
	RelaunchOf *uuid.UUID
}

// LaunchInventorySync queues a sync of an inventory from its source. It
// returns a LaunchError if the inventory has no source or the source can't
// be used, and ErrSyncActive if a sync is already pending or running.
func (s *Store) LaunchInventorySync(ctx context.Context, inventoryID uuid.UUID, in NewSync) (Job, error) {
	var out Job
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		id, err := launchSync(ctx, tx, inventoryID, in)
		if err != nil {
			return err
		}
		out, err = getJob(ctx, tx, id, false)
		return err
	})
	return out, err
}

func launchSync(ctx context.Context, tx pgx.Tx, inventoryID uuid.UUID, in NewSync) (uuid.UUID, error) {
	// Locking the source serializes launches, so the check below holds.
	src, err := getInventorySource(ctx, tx, inventoryID, true)
	if errors.Is(err, ErrNotFound) {
		return uuid.Nil, &LaunchError{"the inventory is not synced from a source"}
	}
	if err != nil {
		return uuid.Nil, err
	}
	var active bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM jobs WHERE sync_inventory_id = $1 AND status IN ('pending', 'waiting', 'running'))`,
		inventoryID).Scan(&active); err != nil {
		return uuid.Nil, err
	}
	if active {
		return uuid.Nil, ErrSyncActive
	}
	inv, err := getInventory(ctx, tx, inventoryID)
	if err != nil {
		return uuid.Nil, err
	}
	p, err := getProject(ctx, tx, src.ProjectID)
	if err != nil {
		return uuid.Nil, err
	}
	ee, err := getExecutionEnvironment(ctx, tx, src.ExecutionEnvironmentID)
	if err != nil {
		return uuid.Nil, err
	}
	cfg := JobConfig{
		JobType: JobTypeInventorySync,
		ExecutionEnvironment: JobExecutionEnvironment{
			ID: ee.ID, Name: ee.Name, Image: ee.Image, PullPolicy: ee.PullPolicy, RegistryCredentialID: ee.RegistryCredentialID,
		},
		Project:        &JobProject{ID: p.ID, Name: p.Name, ScmURL: p.ScmURL, ScmRef: p.ScmRef, CredentialID: p.CredentialID},
		Entrypoint:     src.SourcePath,
		CredentialIDs:  nonNilUUIDs(src.CredentialIDs),
		Command:        []string{},
		ExtraVars:      map[string]any{},
		Verbosity:      src.Verbosity,
		TimeoutSeconds: src.TimeoutSeconds,
	}
	jobID, err := createJob(ctx, tx, NewJob{
		Name: "Sync " + inv.Name, LaunchType: in.LaunchType, LaunchedBy: in.LaunchedBy, Config: cfg,
		ScheduleID: in.ScheduleID, RelaunchOf: in.RelaunchOf, SyncInventoryID: &inventoryID,
	})
	if errors.Is(err, ErrConflict) {
		return uuid.Nil, ErrSyncActive
	}
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE inventory_sources SET last_job_id = $2 WHERE inventory_id = $1`, inventoryID, jobID); err != nil {
		return uuid.Nil, err
	}
	return jobID, nil
}

// --- Sync results ------------------------------------------------------------

// Limits on what one sync may bring in.
const (
	MaxSyncHosts  = 100_000
	MaxSyncGroups = 100_000
)

// SyncedInventory is the content of an inventory as ansible-inventory
// reports it, ready to replace a stored inventory's hosts and groups.
type SyncedInventory struct {
	// Hosts maps each host name to its variables.
	Hosts map[string]map[string]any
	// Groups maps each group name (without all and ungrouped) to its content.
	Groups map[string]*SyncedGroup
	// Warnings are about parts of the input that were left out.
	Warnings []string
}

// SyncedGroup is one group of a SyncedInventory.
type SyncedGroup struct {
	Vars     map[string]any
	Hosts    []string
	Children []string
}

// ParseAnsibleInventory reads the output of `ansible-inventory --list
// --export`:
//
//	{"_meta": {"hostvars": {host: vars}}, "all": {"children": [...]},
//	 group: {"hosts": [...], "children": [...], "vars": {...}}, ...}
//
// The all group's variables are ignored: the inventory's own variables
// stay as they are. Groups named only as children are created too. Host
// names containing whitespace are left out, with a warning.
func ParseAnsibleInventory(data map[string]any) (SyncedInventory, error) {
	out := SyncedInventory{Hosts: map[string]map[string]any{}, Groups: map[string]*SyncedGroup{}}
	skipped := map[string]bool{}
	addHost := func(name string) bool {
		if name == "" || strings.ContainsFunc(name, unicode.IsSpace) || len(name) > 255 || hostpattern.CheckHostName(name) != nil {
			if !skipped[name] {
				skipped[name] = true
				out.Warnings = append(out.Warnings, fmt.Sprintf("Skipped host %q: host names can't be empty, contain spaces or commas, "+
					"start with @ (or have @ after a separator), or be longer than 255 characters", name))
			}
			return false
		}
		if _, ok := out.Hosts[name]; !ok {
			out.Hosts[name] = map[string]any{}
		}
		return true
	}
	pseudo := func(name string) bool { return name == "all" || name == "ungrouped" || name == "_meta" }
	group := func(name string) *SyncedGroup {
		g, ok := out.Groups[name]
		if !ok {
			g = &SyncedGroup{Vars: map[string]any{}}
			out.Groups[name] = g
		}
		return g
	}

	meta, _ := data["_meta"].(map[string]any)
	if hv, ok := meta["hostvars"].(map[string]any); ok {
		for name, vars := range hv {
			if !addHost(name) {
				continue
			}
			if m, ok := vars.(map[string]any); ok {
				out.Hosts[name] = m
			}
		}
	}

	for name, raw := range data {
		if name == "_meta" {
			continue
		}
		def, ok := raw.(map[string]any)
		if !ok {
			return out, fmt.Errorf("group %q is not an object", name)
		}
		hosts := stringList(def["hosts"])
		children := stringList(def["children"])
		if name == "all" || name == "ungrouped" {
			for _, h := range hosts {
				addHost(h)
			}
			for _, c := range children {
				if !pseudo(c) {
					group(c)
				}
			}
			continue
		}
		if strings.TrimSpace(name) == "" || len(name) > 255 {
			out.Warnings = append(out.Warnings, fmt.Sprintf("Skipped group %q: group names can't be blank or longer than 255 characters", name))
			continue
		}
		g := group(name)
		if v, ok := def["vars"].(map[string]any); ok {
			g.Vars = v
		}
		for _, h := range hosts {
			if addHost(h) {
				g.Hosts = append(g.Hosts, h)
			}
		}
		for _, c := range children {
			if !pseudo(c) && c != name && strings.TrimSpace(c) != "" && len(c) <= 255 {
				group(c)
				g.Children = append(g.Children, c)
			}
		}
	}

	if len(out.Hosts) > MaxSyncHosts {
		return out, fmt.Errorf("the inventory has %d hosts; at most %d can be synced", len(out.Hosts), MaxSyncHosts)
	}
	if len(out.Groups) > MaxSyncGroups {
		return out, fmt.Errorf("the inventory has %d groups; at most %d can be synced", len(out.Groups), MaxSyncGroups)
	}
	sort.Strings(out.Warnings)
	return out, nil
}

func stringList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// SyncCounts says what a sync changed.
type SyncCounts struct {
	HostsAdded, HostsUpdated, HostsRemoved    int
	GroupsAdded, GroupsUpdated, GroupsRemoved int
}

// ReplaceInventoryContents replaces the hosts and groups of the inventory
// a running sync job syncs with inv, in one transaction. Hosts and groups
// keep their IDs when their names stay. It returns ErrJobState if jobID is
// not a running sync of inventoryID.
func (s *Store) ReplaceInventoryContents(ctx context.Context, jobID, inventoryID uuid.UUID, inv SyncedInventory) (SyncCounts, error) {
	var counts SyncCounts
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockInventory(ctx, tx, inventoryID); err != nil {
			return err
		}
		var running bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1 AND sync_inventory_id = $2 AND status = 'running')`,
			jobID, inventoryID).Scan(&running); err != nil {
			return err
		}
		if !running {
			return ErrJobState
		}

		// Hosts.
		names, vars, err := namesAndVars(inv.Hosts)
		if err != nil {
			return err
		}
		counts.HostsAdded, counts.HostsUpdated, err = upsertNamed(ctx, tx, "hosts", inventoryID, names, vars)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM hosts WHERE inventory_id = $1 AND name <> ALL($2::text[])`, inventoryID, names)
		if err != nil {
			return err
		}
		counts.HostsRemoved = int(tag.RowsAffected())

		// Groups.
		groupVars := make(map[string]map[string]any, len(inv.Groups))
		for name, g := range inv.Groups {
			groupVars[name] = g.Vars
		}
		gnames, gvars, err := namesAndVars(groupVars)
		if err != nil {
			return err
		}
		counts.GroupsAdded, counts.GroupsUpdated, err = upsertNamed(ctx, tx, "inventory_groups", inventoryID, gnames, gvars)
		if err != nil {
			return err
		}
		tag, err = tx.Exec(ctx, `DELETE FROM inventory_groups WHERE inventory_id = $1 AND name <> ALL($2::text[])`, inventoryID, gnames)
		if err != nil {
			return err
		}
		counts.GroupsRemoved = int(tag.RowsAffected())

		// Memberships and children, rebuilt from scratch.
		var mg, mh, cp, cc []string
		for _, name := range gnames {
			g := inv.Groups[name]
			for _, h := range g.Hosts {
				mg, mh = append(mg, name), append(mh, h)
			}
			for _, c := range g.Children {
				cp, cc = append(cp, name), append(cc, c)
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM inventory_group_hosts WHERE inventory_id = $1`, inventoryID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO inventory_group_hosts (inventory_id, group_id, host_id)
			SELECT $1, g.id, h.id FROM unnest($2::text[], $3::text[]) AS m(gname, hname)
			JOIN inventory_groups g ON g.inventory_id = $1 AND g.name = m.gname
			JOIN hosts h ON h.inventory_id = $1 AND h.name = m.hname
			ON CONFLICT DO NOTHING`, inventoryID, mg, mh); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM inventory_group_children WHERE inventory_id = $1`, inventoryID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO inventory_group_children (inventory_id, parent_id, child_id)
			SELECT $1, p.id, c.id FROM unnest($2::text[], $3::text[]) AS m(pname, cname)
			JOIN inventory_groups p ON p.inventory_id = $1 AND p.name = m.pname
			JOIN inventory_groups c ON c.inventory_id = $1 AND c.name = m.cname
			WHERE p.id <> c.id
			ON CONFLICT DO NOTHING`, inventoryID, cp, cc); err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `UPDATE inventory_sources SET last_synced_at = now(), last_job_id = $2 WHERE inventory_id = $1`,
			inventoryID, jobID)
		return err
	})
	return counts, err
}

// namesAndVars returns m's names, sorted, and their variables as JSON text.
func namesAndVars(m map[string]map[string]any) ([]string, []string, error) {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	slices.Sort(names)
	vars := make([]string, len(names))
	for i, name := range names {
		b, err := json.Marshal(nonNil(m[name]))
		if err != nil {
			return nil, nil, fmt.Errorf("variables of %q: %w", name, err)
		}
		vars[i] = string(b)
	}
	return names, vars, nil
}

// upsertNamed inserts the named rows of an inventory's hosts or groups
// table, or updates the variables of those that exist, and counts the
// rows added and those whose variables changed. Synced hosts are enabled.
func upsertNamed(ctx context.Context, tx pgx.Tx, table string, inventoryID uuid.UUID, names, vars []string) (added, updated int, err error) {
	extra, extraVal, extraChanged := "", "", ""
	if table == "hosts" {
		extra, extraVal, extraChanged = ", enabled", ", true", ", enabled = true"
	}
	changed := table + `.variables IS DISTINCT FROM EXCLUDED.variables`
	if table == "hosts" {
		changed += ` OR NOT hosts.enabled`
	}
	rows, err := tx.Query(ctx, `
		INSERT INTO `+table+` (inventory_id, name, variables`+extra+`)
		SELECT $1, n, v::jsonb`+extraVal+` FROM unnest($2::text[], $3::text[]) AS t(n, v)
		ON CONFLICT (inventory_id, name) DO UPDATE SET variables = EXCLUDED.variables`+extraChanged+`
		WHERE `+changed+`
		RETURNING xmax = 0`, inventoryID, names, vars)
	if err != nil {
		return 0, 0, mapErr(err)
	}
	inserted, err := pgx.CollectRows(rows, pgx.RowTo[bool])
	if err != nil {
		return 0, 0, err
	}
	for _, ins := range inserted {
		if ins {
			added++
		} else {
			updated++
		}
	}
	return added, updated, nil
}
