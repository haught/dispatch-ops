package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrCycle means a group change would make a group its own descendant.
var ErrCycle = errors.New("group hierarchy would contain a cycle")

// Inventory is a row in inventories, with host and group counts.
type Inventory struct {
	ID          uuid.UUID
	Name        string
	Description string
	Variables   map[string]any
	HostCount   int
	GroupCount  int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const inventoryColumns = `i.id, i.name, i.description, i.variables,
	(SELECT count(*) FROM hosts WHERE inventory_id = i.id),
	(SELECT count(*) FROM inventory_groups WHERE inventory_id = i.id),
	i.created_at, i.updated_at`

func (i *Inventory) scanTargets() []any {
	return []any{&i.ID, &i.Name, &i.Description, &i.Variables, &i.HostCount, &i.GroupCount, &i.CreatedAt, &i.UpdatedAt}
}

// InventoryFields are an inventory's editable fields.
type InventoryFields struct {
	Name        string
	Description string
	Variables   map[string]any
}

// ListInventories returns inventories whose name contains search, by name.
func (s *Store) ListInventories(ctx context.Context, search string, page Page) ([]Inventory, int, error) {
	return list(ctx, s.pool, listQuery{
		columns: inventoryColumns, from: `inventories i`,
		where: `i.name ILIKE $1`, args: []any{likePattern(search)}, orderBy: `i.name`,
	}, page, scanner[Inventory]())
}

// GetInventory returns one inventory.
func (s *Store) GetInventory(ctx context.Context, id uuid.UUID) (Inventory, error) {
	return getInventory(ctx, s.pool, id)
}

func getInventory(ctx context.Context, db querier, id uuid.UUID) (Inventory, error) {
	var i Inventory
	err := db.QueryRow(ctx, `SELECT `+inventoryColumns+` FROM inventories i WHERE i.id = $1`, id).Scan(i.scanTargets()...)
	return i, mapErr(err)
}

// CreateInventory inserts an inventory.
func (s *Store) CreateInventory(ctx context.Context, in InventoryFields) (Inventory, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`INSERT INTO inventories (name, description, variables) VALUES ($1, $2, $3) RETURNING id`,
		in.Name, in.Description, nonNil(in.Variables)).Scan(&id)
	if err != nil {
		return Inventory{}, mapErr(err)
	}
	return s.GetInventory(ctx, id)
}

// UpdateInventory loads an inventory's fields, lets update change them, and
// saves the result, all in one transaction.
func (s *Store) UpdateInventory(ctx context.Context, id uuid.UUID, update func(*InventoryFields) error) (Inventory, error) {
	var out Inventory
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var f InventoryFields
		err := tx.QueryRow(ctx, `SELECT name, description, variables FROM inventories WHERE id = $1 FOR UPDATE`, id).
			Scan(&f.Name, &f.Description, &f.Variables)
		if err != nil {
			return mapErr(err)
		}
		if err := update(&f); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE inventories SET name = $2, description = $3, variables = $4 WHERE id = $1`,
			id, f.Name, f.Description, nonNil(f.Variables)); err != nil {
			return mapErr(err)
		}
		out, err = getInventory(ctx, tx, id)
		return err
	})
	return out, err
}

// DeleteInventory deletes an inventory and its hosts and groups. It returns
// ErrInUse if a job template uses it.
func (s *Store) DeleteInventory(ctx context.Context, id uuid.UUID) error {
	return s.deleteByID(ctx, "inventories", id)
}

// --- Hosts -------------------------------------------------------------------

// Host is a row in hosts, with its direct group memberships.
type Host struct {
	ID          uuid.UUID
	InventoryID uuid.UUID
	Name        string
	Description string
	Variables   map[string]any
	Enabled     bool
	GroupIDs    []uuid.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const hostColumns = `h.id, h.inventory_id, h.name, h.description, h.variables, h.enabled,
	ARRAY(SELECT gh.group_id FROM inventory_group_hosts gh WHERE gh.host_id = h.id ORDER BY gh.group_id),
	h.created_at, h.updated_at`

func (h *Host) scanTargets() []any {
	return []any{&h.ID, &h.InventoryID, &h.Name, &h.Description, &h.Variables, &h.Enabled, &h.GroupIDs, &h.CreatedAt, &h.UpdatedAt}
}

// HostFields are a host's editable fields.
type HostFields struct {
	Name        string
	Description string
	Variables   map[string]any
	Enabled     bool
	GroupIDs    []uuid.UUID
}

// HostFilter narrows ListHosts.
type HostFilter struct {
	Search string
	// GroupID, if set, selects direct members of that group.
	GroupID *uuid.UUID
}

// ListHosts returns an inventory's hosts, by name. It returns ErrNotFound if
// the inventory does not exist.
func (s *Store) ListHosts(ctx context.Context, inventoryID uuid.UUID, f HostFilter, page Page) ([]Host, int, error) {
	if err := s.inventoryExists(ctx, inventoryID); err != nil {
		return nil, 0, err
	}
	q := listQuery{
		columns: hostColumns, from: `hosts h`,
		where: `h.inventory_id = $1 AND h.name ILIKE $2`, args: []any{inventoryID, likePattern(f.Search)},
		orderBy: `h.name`,
	}
	if f.GroupID != nil {
		q.where += ` AND EXISTS (SELECT 1 FROM inventory_group_hosts gh WHERE gh.host_id = h.id AND gh.group_id = $3)`
		q.args = append(q.args, *f.GroupID)
	}
	return list(ctx, s.pool, q, page, scanner[Host]())
}

func (s *Store) inventoryExists(ctx context.Context, id uuid.UUID) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM inventories WHERE id = $1)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

// GetHost returns one host.
func (s *Store) GetHost(ctx context.Context, id uuid.UUID) (Host, error) {
	return getHost(ctx, s.pool, id)
}

func getHost(ctx context.Context, db querier, id uuid.UUID) (Host, error) {
	var h Host
	err := db.QueryRow(ctx, `SELECT `+hostColumns+` FROM hosts h WHERE h.id = $1`, id).Scan(h.scanTargets()...)
	return h, mapErr(err)
}

// CreateHost adds a host to an inventory. It returns ErrNotFound if the
// inventory does not exist, and ErrInvalidReference if a group is not in it.
func (s *Store) CreateHost(ctx context.Context, inventoryID uuid.UUID, in HostFields) (Host, error) {
	var out Host
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `
			INSERT INTO hosts (inventory_id, name, description, variables, enabled)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			inventoryID, in.Name, in.Description, nonNil(in.Variables), in.Enabled).Scan(&id)
		if err != nil {
			if err = mapErr(err); errors.Is(err, ErrInvalidReference) {
				return ErrNotFound
			}
			return err
		}
		if err := setHostGroups(ctx, tx, inventoryID, id, in.GroupIDs); err != nil {
			return err
		}
		out, err = getHost(ctx, tx, id)
		return err
	})
	return out, err
}

// UpdateHost loads a host's fields, lets update change them, and saves the
// result. Group memberships are replaced with the updated GroupIDs.
func (s *Store) UpdateHost(ctx context.Context, id uuid.UUID, update func(*HostFields) error) (Host, error) {
	var out Host
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM hosts WHERE id = $1 FOR UPDATE`, id); err != nil {
			return err
		}
		h, err := getHost(ctx, tx, id)
		if err != nil {
			return err
		}
		f := HostFields{Name: h.Name, Description: h.Description, Variables: h.Variables, Enabled: h.Enabled, GroupIDs: h.GroupIDs}
		if err := update(&f); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE hosts SET name = $2, description = $3, variables = $4, enabled = $5 WHERE id = $1`,
			id, f.Name, f.Description, nonNil(f.Variables), f.Enabled); err != nil {
			return mapErr(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM inventory_group_hosts WHERE host_id = $1`, id); err != nil {
			return err
		}
		if err := setHostGroups(ctx, tx, h.InventoryID, id, f.GroupIDs); err != nil {
			return err
		}
		out, err = getHost(ctx, tx, id)
		return err
	})
	return out, err
}

// setHostGroups adds a host to groups. The composite foreign keys reject
// groups from another inventory.
func setHostGroups(ctx context.Context, tx pgx.Tx, inventoryID, hostID uuid.UUID, groupIDs []uuid.UUID) error {
	if len(groupIDs) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO inventory_group_hosts (inventory_id, group_id, host_id)
		SELECT $1, g, $2 FROM unnest($3::uuid[]) AS g
		ON CONFLICT DO NOTHING`, inventoryID, hostID, groupIDs)
	return mapErr(err)
}

// DeleteHost deletes a host.
func (s *Store) DeleteHost(ctx context.Context, id uuid.UUID) error {
	return s.deleteByID(ctx, "hosts", id)
}

// --- Groups ------------------------------------------------------------------

// Group is a row in inventory_groups, with its direct children.
type Group struct {
	ID          uuid.UUID
	InventoryID uuid.UUID
	Name        string
	Description string
	Variables   map[string]any
	ChildIDs    []uuid.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const groupColumns = `g.id, g.inventory_id, g.name, g.description, g.variables,
	ARRAY(SELECT c.child_id FROM inventory_group_children c WHERE c.parent_id = g.id ORDER BY c.child_id),
	g.created_at, g.updated_at`

func (g *Group) scanTargets() []any {
	return []any{&g.ID, &g.InventoryID, &g.Name, &g.Description, &g.Variables, &g.ChildIDs, &g.CreatedAt, &g.UpdatedAt}
}

// GroupFields are a group's editable fields.
type GroupFields struct {
	Name        string
	Description string
	Variables   map[string]any
	ChildIDs    []uuid.UUID
}

// ListGroups returns an inventory's groups, by name.
func (s *Store) ListGroups(ctx context.Context, inventoryID uuid.UUID, search string, page Page) ([]Group, int, error) {
	if err := s.inventoryExists(ctx, inventoryID); err != nil {
		return nil, 0, err
	}
	return list(ctx, s.pool, listQuery{
		columns: groupColumns, from: `inventory_groups g`,
		where: `g.inventory_id = $1 AND g.name ILIKE $2`, args: []any{inventoryID, likePattern(search)},
		orderBy: `g.name`,
	}, page, scanner[Group]())
}

// GetGroup returns one group.
func (s *Store) GetGroup(ctx context.Context, id uuid.UUID) (Group, error) {
	return getGroup(ctx, s.pool, id)
}

func getGroup(ctx context.Context, db querier, id uuid.UUID) (Group, error) {
	var g Group
	err := db.QueryRow(ctx, `SELECT `+groupColumns+` FROM inventory_groups g WHERE g.id = $1`, id).Scan(g.scanTargets()...)
	return g, mapErr(err)
}

// CreateGroup adds a group to an inventory. It returns ErrNotFound if the
// inventory does not exist, and ErrInvalidReference if a child is not in it.
func (s *Store) CreateGroup(ctx context.Context, inventoryID uuid.UUID, in GroupFields) (Group, error) {
	var out Group
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockInventory(ctx, tx, inventoryID); err != nil {
			return err
		}
		var id uuid.UUID
		err := tx.QueryRow(ctx, `
			INSERT INTO inventory_groups (inventory_id, name, description, variables)
			VALUES ($1, $2, $3, $4) RETURNING id`,
			inventoryID, in.Name, in.Description, nonNil(in.Variables)).Scan(&id)
		if err != nil {
			return mapErr(err)
		}
		// A new group has no parents, so its children cannot form a cycle.
		if err := setGroupChildren(ctx, tx, inventoryID, id, in.ChildIDs); err != nil {
			return err
		}
		out, err = getGroup(ctx, tx, id)
		return err
	})
	return out, err
}

// UpdateGroup loads a group's fields, lets update change them, and saves the
// result. Children are replaced with the updated ChildIDs; ErrCycle is
// returned if that would create a cycle.
func (s *Store) UpdateGroup(ctx context.Context, id uuid.UUID, update func(*GroupFields) error) (Group, error) {
	var out Group
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// Changes to the group graph are serialized per inventory, so two
		// concurrent updates cannot each add half of a cycle.
		var inventoryID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT inventory_id FROM inventory_groups WHERE id = $1`, id).Scan(&inventoryID); err != nil {
			return mapErr(err)
		}
		if err := lockInventory(ctx, tx, inventoryID); err != nil {
			return err
		}
		g, err := getGroup(ctx, tx, id)
		if err != nil {
			return err
		}
		f := GroupFields{Name: g.Name, Description: g.Description, Variables: g.Variables, ChildIDs: g.ChildIDs}
		if err := update(&f); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE inventory_groups SET name = $2, description = $3, variables = $4 WHERE id = $1`,
			id, f.Name, f.Description, nonNil(f.Variables)); err != nil {
			return mapErr(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM inventory_group_children WHERE parent_id = $1`, id); err != nil {
			return err
		}
		if err := setGroupChildren(ctx, tx, g.InventoryID, id, f.ChildIDs); err != nil {
			return err
		}
		var cycle bool
		err = tx.QueryRow(ctx, `
			WITH RECURSIVE descendants(id) AS (
				SELECT child_id FROM inventory_group_children WHERE parent_id = $1
				UNION
				SELECT c.child_id FROM inventory_group_children c JOIN descendants d ON c.parent_id = d.id
			)
			SELECT EXISTS (SELECT 1 FROM descendants WHERE id = $1)`, id).Scan(&cycle)
		if err != nil {
			return err
		}
		if cycle {
			return ErrCycle
		}
		out, err = getGroup(ctx, tx, id)
		return err
	})
	return out, err
}

func lockInventory(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	var locked uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM inventories WHERE id = $1 FOR UPDATE`, id).Scan(&locked)
	return mapErr(err)
}

func setGroupChildren(ctx context.Context, tx pgx.Tx, inventoryID, parentID uuid.UUID, childIDs []uuid.UUID) error {
	if len(childIDs) == 0 {
		return nil
	}
	for _, c := range childIDs {
		if c == parentID {
			return ErrCycle
		}
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO inventory_group_children (inventory_id, parent_id, child_id)
		SELECT $1, $2, c FROM unnest($3::uuid[]) AS c
		ON CONFLICT DO NOTHING`, inventoryID, parentID, childIDs)
	return mapErr(err)
}

// DeleteGroup deletes a group. Its hosts and child groups are kept.
func (s *Store) DeleteGroup(ctx context.Context, id uuid.UUID) error {
	return s.deleteByID(ctx, "inventory_groups", id)
}

// --- Export ------------------------------------------------------------------

// InventorySnapshot is everything in an inventory that a job needs.
type InventorySnapshot struct {
	Variables map[string]any
	Groups    []snapshotGroup
	Hosts     []snapshotHost
}

type snapshotGroup struct {
	Name      string
	Variables map[string]any
	Children  []string
	Hosts     []string
}

type snapshotHost struct {
	Name      string
	Variables map[string]any
	Grouped   bool
}

// GetInventorySnapshot reads an inventory's variables, groups and enabled
// hosts in one consistent snapshot.
func (s *Store) GetInventorySnapshot(ctx context.Context, id uuid.UUID) (InventorySnapshot, error) {
	var snap InventorySnapshot
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT variables FROM inventories WHERE id = $1`, id).Scan(&snap.Variables); err != nil {
			return mapErr(err)
		}
		rows, err := tx.Query(ctx, `
			SELECT g.name, g.variables,
				ARRAY(SELECT cg.name FROM inventory_group_children c JOIN inventory_groups cg ON cg.id = c.child_id
				      WHERE c.parent_id = g.id ORDER BY cg.name),
				ARRAY(SELECT h.name FROM inventory_group_hosts gh JOIN hosts h ON h.id = gh.host_id
				      WHERE gh.group_id = g.id AND h.enabled ORDER BY h.name)
			FROM inventory_groups g WHERE g.inventory_id = $1 ORDER BY g.name`, id)
		if err != nil {
			return err
		}
		snap.Groups, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (snapshotGroup, error) {
			var g snapshotGroup
			err := row.Scan(&g.Name, &g.Variables, &g.Children, &g.Hosts)
			return g, err
		})
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `
			SELECT h.name, h.variables, EXISTS (SELECT 1 FROM inventory_group_hosts gh WHERE gh.host_id = h.id)
			FROM hosts h WHERE h.inventory_id = $1 AND h.enabled ORDER BY h.name`, id)
		if err != nil {
			return err
		}
		snap.Hosts, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (snapshotHost, error) {
			var h snapshotHost
			err := row.Scan(&h.Name, &h.Variables, &h.Grouped)
			return h, err
		})
		return err
	})
	return snap, err
}

// Ansible returns the snapshot in Ansible's JSON inventory format.
func (snap InventorySnapshot) Ansible() map[string]any {
	out := map[string]any{}
	isChild := map[string]bool{}
	for _, g := range snap.Groups {
		for _, c := range g.Children {
			isChild[c] = true
		}
	}

	topLevel := []string{"ungrouped"}
	for _, g := range snap.Groups {
		entry := map[string]any{}
		if len(g.Hosts) > 0 {
			entry["hosts"] = g.Hosts
		}
		if len(g.Variables) > 0 {
			entry["vars"] = g.Variables
		}
		if len(g.Children) > 0 {
			entry["children"] = g.Children
		}
		out[g.Name] = entry
		if !isChild[g.Name] {
			topLevel = append(topLevel, g.Name)
		}
	}

	ungrouped := []string{}
	hostvars := map[string]any{}
	for _, h := range snap.Hosts {
		if !h.Grouped {
			ungrouped = append(ungrouped, h.Name)
		}
		hostvars[h.Name] = nonNil(h.Variables)
	}
	out["ungrouped"] = map[string]any{"hosts": ungrouped}

	all := map[string]any{"children": topLevel}
	if len(snap.Variables) > 0 {
		all["vars"] = snap.Variables
	}
	out["all"] = all
	out["_meta"] = map[string]any{"hostvars": hostvars}
	return out
}
