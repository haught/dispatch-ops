package api

import (
	"context"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/store"
)

// pageOf turns the shared limit/offset query parameters into a store.Page.
// The spec validates their ranges.
func pageOf(limit *apigen.Limit, offset *apigen.Offset) store.Page {
	p := store.Page{Limit: 50}
	if limit != nil {
		p.Limit = *limit
	}
	if offset != nil {
		p.Offset = *offset
	}
	return p
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func vars(v *apigen.Variables) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	return *v
}

func (h *Handlers) ListInventories(ctx context.Context, req apigen.ListInventoriesRequestObject) (apigen.ListInventoriesResponseObject, error) {
	items, total, err := h.Store.ListInventories(ctx, deref(req.Params.Search), pageOf(req.Params.Limit, req.Params.Offset))
	if err != nil {
		return nil, err
	}
	out := apigen.ListInventories200JSONResponse{Items: make([]apigen.Inventory, 0, len(items)), Total: total}
	for _, i := range items {
		out.Items = append(out.Items, toAPIInventory(i))
	}
	return out, nil
}

func (h *Handlers) CreateInventory(ctx context.Context, req apigen.CreateInventoryRequestObject) (apigen.CreateInventoryResponseObject, error) {
	inv, err := h.Store.CreateInventory(ctx, store.InventoryFields{
		Name: req.Body.Name, Description: deref(req.Body.Description), Variables: vars(req.Body.Variables),
	})
	if err != nil {
		return nil, err
	}
	return apigen.CreateInventory201JSONResponse(toAPIInventory(inv)), nil
}

func (h *Handlers) GetInventory(ctx context.Context, req apigen.GetInventoryRequestObject) (apigen.GetInventoryResponseObject, error) {
	inv, err := h.Store.GetInventory(ctx, req.InventoryId)
	if err != nil {
		return nil, orNotFound(err, "Inventory")
	}
	return apigen.GetInventory200JSONResponse(toAPIInventory(inv)), nil
}

func (h *Handlers) UpdateInventory(ctx context.Context, req apigen.UpdateInventoryRequestObject) (apigen.UpdateInventoryResponseObject, error) {
	b := req.Body
	inv, err := h.Store.UpdateInventory(ctx, req.InventoryId, func(f *store.InventoryFields) error {
		setIf(&f.Name, b.Name)
		setIf(&f.Description, b.Description)
		if b.Variables != nil {
			f.Variables = *b.Variables
		}
		return nil
	})
	if err != nil {
		return nil, orNotFound(err, "Inventory")
	}
	return apigen.UpdateInventory200JSONResponse(toAPIInventory(inv)), nil
}

func (h *Handlers) DeleteInventory(ctx context.Context, req apigen.DeleteInventoryRequestObject) (apigen.DeleteInventoryResponseObject, error) {
	if err := h.Store.DeleteInventory(ctx, req.InventoryId); err != nil {
		return nil, orNotFound(err, "Inventory")
	}
	return apigen.DeleteInventory204Response{}, nil
}

func (h *Handlers) ExportInventory(ctx context.Context, req apigen.ExportInventoryRequestObject) (apigen.ExportInventoryResponseObject, error) {
	snap, err := h.Store.GetInventorySnapshot(ctx, req.InventoryId)
	if err != nil {
		return nil, orNotFound(err, "Inventory")
	}
	return apigen.ExportInventory200JSONResponse(snap.Ansible()), nil
}

// --- Hosts ---

func (h *Handlers) ListHosts(ctx context.Context, req apigen.ListHostsRequestObject) (apigen.ListHostsResponseObject, error) {
	p := req.Params
	items, total, err := h.Store.ListHosts(ctx, req.InventoryId,
		store.HostFilter{Search: deref(p.Search), GroupID: p.GroupId}, pageOf(p.Limit, p.Offset))
	if err != nil {
		return nil, orNotFound(err, "Inventory")
	}
	out := apigen.ListHosts200JSONResponse{Items: make([]apigen.Host, 0, len(items)), Total: total}
	for _, host := range items {
		out.Items = append(out.Items, toAPIHost(host))
	}
	return out, nil
}

func (h *Handlers) CreateHost(ctx context.Context, req apigen.CreateHostRequestObject) (apigen.CreateHostResponseObject, error) {
	b := req.Body
	enabled := true
	setIf(&enabled, b.Enabled)
	host, err := h.Store.CreateHost(ctx, req.InventoryId, store.HostFields{
		Name: b.Name, Description: deref(b.Description), Variables: vars(b.Variables), Enabled: enabled,
		GroupIDs: deref(b.GroupIds),
	})
	if err != nil {
		return nil, orNotFound(err, "Inventory")
	}
	return apigen.CreateHost201JSONResponse(toAPIHost(host)), nil
}

func (h *Handlers) GetHost(ctx context.Context, req apigen.GetHostRequestObject) (apigen.GetHostResponseObject, error) {
	host, err := h.Store.GetHost(ctx, req.HostId)
	if err != nil {
		return nil, orNotFound(err, "Host")
	}
	return apigen.GetHost200JSONResponse(toAPIHost(host)), nil
}

func (h *Handlers) UpdateHost(ctx context.Context, req apigen.UpdateHostRequestObject) (apigen.UpdateHostResponseObject, error) {
	b := req.Body
	host, err := h.Store.UpdateHost(ctx, req.HostId, func(f *store.HostFields) error {
		setIf(&f.Name, b.Name)
		setIf(&f.Description, b.Description)
		setIf(&f.Enabled, b.Enabled)
		setIf(&f.GroupIDs, b.GroupIds)
		if b.Variables != nil {
			f.Variables = *b.Variables
		}
		return nil
	})
	if err != nil {
		return nil, orNotFound(err, "Host")
	}
	return apigen.UpdateHost200JSONResponse(toAPIHost(host)), nil
}

func (h *Handlers) DeleteHost(ctx context.Context, req apigen.DeleteHostRequestObject) (apigen.DeleteHostResponseObject, error) {
	if err := h.Store.DeleteHost(ctx, req.HostId); err != nil {
		return nil, orNotFound(err, "Host")
	}
	return apigen.DeleteHost204Response{}, nil
}

// --- Groups ---

// reservedGroupNames are implicit in every Ansible inventory.
var reservedGroupNames = map[string]bool{"all": true, "ungrouped": true}

func checkGroupName(name string) error {
	if reservedGroupNames[name] {
		return badRequest("%q is a reserved group name", name)
	}
	return nil
}

func (h *Handlers) ListGroups(ctx context.Context, req apigen.ListGroupsRequestObject) (apigen.ListGroupsResponseObject, error) {
	p := req.Params
	items, total, err := h.Store.ListGroups(ctx, req.InventoryId, deref(p.Search), pageOf(p.Limit, p.Offset))
	if err != nil {
		return nil, orNotFound(err, "Inventory")
	}
	out := apigen.ListGroups200JSONResponse{Items: make([]apigen.Group, 0, len(items)), Total: total}
	for _, g := range items {
		out.Items = append(out.Items, toAPIGroup(g))
	}
	return out, nil
}

func (h *Handlers) CreateGroup(ctx context.Context, req apigen.CreateGroupRequestObject) (apigen.CreateGroupResponseObject, error) {
	b := req.Body
	if err := checkGroupName(b.Name); err != nil {
		return nil, err
	}
	g, err := h.Store.CreateGroup(ctx, req.InventoryId, store.GroupFields{
		Name: b.Name, Description: deref(b.Description), Variables: vars(b.Variables), ChildIDs: deref(b.ChildIds),
	})
	if err != nil {
		return nil, orNotFound(err, "Inventory")
	}
	return apigen.CreateGroup201JSONResponse(toAPIGroup(g)), nil
}

func (h *Handlers) GetGroup(ctx context.Context, req apigen.GetGroupRequestObject) (apigen.GetGroupResponseObject, error) {
	g, err := h.Store.GetGroup(ctx, req.GroupId)
	if err != nil {
		return nil, orNotFound(err, "Group")
	}
	return apigen.GetGroup200JSONResponse(toAPIGroup(g)), nil
}

func (h *Handlers) UpdateGroup(ctx context.Context, req apigen.UpdateGroupRequestObject) (apigen.UpdateGroupResponseObject, error) {
	b := req.Body
	if b.Name != nil {
		if err := checkGroupName(*b.Name); err != nil {
			return nil, err
		}
	}
	g, err := h.Store.UpdateGroup(ctx, req.GroupId, func(f *store.GroupFields) error {
		setIf(&f.Name, b.Name)
		setIf(&f.Description, b.Description)
		setIf(&f.ChildIDs, b.ChildIds)
		if b.Variables != nil {
			f.Variables = *b.Variables
		}
		return nil
	})
	if err != nil {
		return nil, orNotFound(err, "Group")
	}
	return apigen.UpdateGroup200JSONResponse(toAPIGroup(g)), nil
}

func (h *Handlers) DeleteGroup(ctx context.Context, req apigen.DeleteGroupRequestObject) (apigen.DeleteGroupResponseObject, error) {
	if err := h.Store.DeleteGroup(ctx, req.GroupId); err != nil {
		return nil, orNotFound(err, "Group")
	}
	return apigen.DeleteGroup204Response{}, nil
}

// --- Conversions ---

func toAPIInventory(i store.Inventory) apigen.Inventory {
	return apigen.Inventory{
		Id: i.ID, Name: i.Name, Description: i.Description, Variables: i.Variables,
		HostCount: i.HostCount, GroupCount: i.GroupCount, CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt,
	}
}

func toAPIHost(h store.Host) apigen.Host {
	return apigen.Host{
		Id: h.ID, InventoryId: h.InventoryID, Name: h.Name, Description: h.Description, Variables: h.Variables,
		Enabled: h.Enabled, GroupIds: nonNilSlice(h.GroupIDs), CreatedAt: h.CreatedAt, UpdatedAt: h.UpdatedAt,
	}
}

func toAPIGroup(g store.Group) apigen.Group {
	return apigen.Group{
		Id: g.ID, InventoryId: g.InventoryID, Name: g.Name, Description: g.Description, Variables: g.Variables,
		ChildIds: nonNilSlice(g.ChildIDs), CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt,
	}
}

// setIf sets *dst to *src when src is present (a PATCH field that was sent).
func setIf[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

func nonNilSlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
