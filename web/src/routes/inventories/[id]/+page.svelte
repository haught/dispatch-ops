<script lang="ts">
	import { tick } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import {
		api,
		errorMessage,
		listTemplates,
		type Group,
		type Host,
		type Inventory,
		type JobTemplate
	} from '#lib/api/client.js';
	import GroupForm from '#lib/components/GroupForm.svelte';
	import HostForm from '#lib/components/HostForm.svelte';
	import InventorySourceCard from '#lib/components/InventorySourceCard.svelte';
	import ResourceAccess from '#lib/components/ResourceAccess.svelte';
	import UsedBy from '#lib/components/UsedBy.svelte';
	import VarsField from '#lib/components/VarsField.svelte';
	import { fmtTime, fmtVars, parseVars, varsError } from '#lib/format.js';
	import { can, loadMyGrants } from '#lib/permissions.svelte.js';

	const id = $derived(page.params.id ?? '');

	let inventory = $state<Inventory | null>(null);
	let groups = $state<Group[]>([]);
	let hosts = $state<Host[]>([]);
	let hostTotal = $state(0);
	let templates = $state<JobTemplate[]>([]);
	let error = $state('');

	let hostSearch = $state('');
	let hostGroup = $state('');
	// The host or group whose form is open: one, 'new', or none.
	let hostOpen = $state<Host | 'new' | null>(null);
	let groupOpen = $state<Group | 'new' | null>(null);
	let hostError = $state('');
	let groupError = $state('');

	let name = $state('');
	let description = $state('');
	let vars = $state('');
	let saving = $state(false);
	let message = $state('');
	let formError = $state('');

	let exported = $state<string | null>(null);

	const canAdmin = $derived(can('inventory', id, 'admin'));
	// A synced inventory's hosts and groups come from its source.
	const synced = $derived(Boolean(inventory?.source));
	const canEditHosts = $derived(canAdmin && !synced);
	const groupName = $derived(new Map(groups.map((g) => [g.id, g.name])));

	function fill(inv: Inventory) {
		inventory = inv;
		name = inv.name;
		description = inv.description;
		vars = fmtVars(inv.variables);
	}

	async function loadInventory(iid: string) {
		const { data, error: err } = await api.GET('/api/v1/inventories/{inventoryId}', {
			params: { path: { inventoryId: iid } }
		});
		if (data) fill(data);
		else error = errorMessage(err);
	}

	async function loadGroups(iid: string) {
		const { data } = await api.GET('/api/v1/inventories/{inventoryId}/groups', {
			params: { path: { inventoryId: iid }, query: { limit: 500 } }
		});
		groups = data?.items ?? [];
	}

	async function loadHosts(iid: string, search: string, groupId: string) {
		const { data, error: err } = await api.GET('/api/v1/inventories/{inventoryId}/hosts', {
			params: {
				path: { inventoryId: iid },
				query: { limit: 500, search: search || undefined, groupId: groupId || undefined }
			}
		});
		if (data) {
			hosts = data.items;
			hostTotal = data.total;
		} else {
			hostError = errorMessage(err);
		}
	}

	$effect(() => {
		const iid = id;
		if (!iid) return;
		inventory = null;
		error = '';
		exported = null;
		hostOpen = groupOpen = null;
		loadInventory(iid);
		loadGroups(iid);
		listTemplates().then((t) => (templates = t.filter((x) => x.inventoryId === iid)));
	});

	$effect(() => {
		if (id) loadHosts(id, hostSearch, hostGroup);
	});

	$effect(() => {
		loadMyGrants();
	});

	async function refresh() {
		await Promise.all([loadInventory(id), loadGroups(id), loadHosts(id, hostSearch, hostGroup)]);
		exported = null;
	}

	async function save(event: SubmitEvent) {
		event.preventDefault();
		if (!inventory) return;
		formError = varsError(vars);
		message = '';
		if (formError) return;
		saving = true;
		const { data, error: err } = await api.PATCH('/api/v1/inventories/{inventoryId}', {
			params: { path: { inventoryId: inventory.id } },
			body: { name, description, variables: parseVars(vars) }
		});
		saving = false;
		if (!data) {
			formError = errorMessage(err);
			return;
		}
		fill(data);
		exported = null;
		message = 'Saved.';
	}

	async function remove() {
		if (!inventory) return;
		if (!confirm(`Delete the inventory ${inventory.name} with its hosts and groups?`)) return;
		error = '';
		const { error: err } = await api.DELETE('/api/v1/inventories/{inventoryId}', {
			params: { path: { inventoryId: inventory.id } }
		});
		if (err) error = errorMessage(err);
		else goto('/inventories');
	}

	async function openHost(h: Host | 'new') {
		hostOpen = h;
		await tick();
		document.getElementById('host-form')?.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
	}

	async function openGroup(g: Group | 'new') {
		groupOpen = g;
		await tick();
		document.getElementById('group-form')?.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
	}

	async function hostSaved() {
		hostOpen = null;
		await refresh();
	}

	async function groupSaved() {
		groupOpen = null;
		await refresh();
	}

	async function removeHost(h: Host) {
		if (!confirm(`Delete the host ${h.name}?`)) return;
		hostError = '';
		const { error: err } = await api.DELETE('/api/v1/hosts/{hostId}', {
			params: { path: { hostId: h.id } }
		});
		if (err) hostError = errorMessage(err);
		if (hostOpen !== 'new' && hostOpen?.id === h.id) hostOpen = null;
		await refresh();
	}

	async function removeGroup(g: Group) {
		if (!confirm(`Delete the group ${g.name}? Its hosts and child groups are kept.`)) return;
		groupError = '';
		const { error: err } = await api.DELETE('/api/v1/groups/{groupId}', {
			params: { path: { groupId: g.id } }
		});
		if (err) groupError = errorMessage(err);
		if (groupOpen !== 'new' && groupOpen?.id === g.id) groupOpen = null;
		if (hostGroup === g.id) hostGroup = '';
		await refresh();
	}

	async function toggleExport() {
		if (!inventory) return;
		if (exported !== null) {
			exported = null;
			return;
		}
		const { data, error: err } = await api.GET('/api/v1/inventories/{inventoryId}/export', {
			params: { path: { inventoryId: inventory.id } }
		});
		if (data) exported = JSON.stringify(data, null, 2);
		else error = errorMessage(err);
	}

	function download() {
		if (!inventory || exported === null) return;
		const url = URL.createObjectURL(new Blob([exported], { type: 'application/json' }));
		const a = document.createElement('a');
		a.href = url;
		a.download = `${inventory.name.replace(/[^\w.-]+/g, '_')}.json`;
		a.click();
		URL.revokeObjectURL(url);
	}

	function parentsOf(g: Group): string[] {
		return groups.filter((p) => p.childIds.includes(g.id)).map((p) => p.name);
	}
</script>

{#if inventory}
	<div class="page-header">
		<h1>{inventory.name}</h1>
		<span class="muted">{inventory.hostCount} hosts · {inventory.groupCount} groups</span>
		<span class="spacer"></span>
		<button class="secondary" onclick={toggleExport}>
			{exported === null ? 'Ansible inventory' : 'Hide Ansible inventory'}
		</button>
		{#if canAdmin}<button class="danger" onclick={remove}>Delete</button>{/if}
	</div>
	{#if inventory.description}<p class="muted">{inventory.description}</p>{/if}
	{#if error}<p class="error" role="alert">{error}</p>{/if}

	{#if exported !== null}
		<section class="card">
			<div class="page-header">
				<h2>Ansible inventory</h2>
				<span class="spacer"></span>
				<button class="secondary" onclick={download}>Download JSON</button>
			</div>
			<p class="muted small">What jobs receive. Disabled hosts are left out.</p>
			<pre class="code">{exported}</pre>
		</section>
	{/if}

	<section class="card">
		{#if canAdmin}
			{#if formError}<p class="error" role="alert">{formError}</p>{/if}
			<form onsubmit={save}>
				<div class="form-grid">
					<label>Name <input bind:value={name} maxlength="255" required /></label>
					<label>Description <input bind:value={description} maxlength="4096" /></label>
				</div>
				<VarsField bind:value={vars} label="Inventory variables" />
				<div class="form-actions">
					<button type="submit" disabled={saving}>{saving ? 'Saving…' : 'Save'}</button>
					{#if message}<span class="muted">{message}</span>{/if}
				</div>
			</form>
		{:else}
			<h2>Inventory variables</h2>
			{#if vars}<pre class="code">{vars}</pre>{:else}<p class="muted">None.</p>{/if}
		{/if}
		<p class="muted small">Updated {fmtTime(inventory.updatedAt)}</p>
	</section>

	<InventorySourceCard {inventory} {canAdmin} onchanged={refresh} />

	<section class="card">
		<div class="page-header">
			<h2>Hosts</h2>
			<span class="spacer"></span>
			{#if canEditHosts}<button onclick={() => openHost('new')}>Add host</button>{/if}
		</div>
		{#if synced}
			<p class="muted small">
				Synced from <code>{inventory.source?.sourcePath}</code>: each sync replaces these hosts and
				groups, so they can't be edited here.
			</p>
		{/if}
		<div class="filters">
			<label>Search <input bind:value={hostSearch} placeholder="Name contains…" /></label>
			<label>
				Group
				<select bind:value={hostGroup}>
					<option value="">Any group</option>
					{#each groups as g (g.id)}
						<option value={g.id}>{g.name}</option>
					{/each}
				</select>
			</label>
		</div>
		{#if hostError}<p class="error" role="alert">{hostError}</p>{/if}
		{#if hosts.length === 0}
			<p class="muted">
				{hostSearch || hostGroup ? 'No hosts match.' : 'No hosts yet.'}
			</p>
		{:else}
			<div class="table-wrap">
				<table>
					<thead>
						<tr>
							<th>Host</th>
							<th>Groups</th>
							<th>Variables</th>
							<th></th>
						</tr>
					</thead>
					<tbody>
						{#each hosts as h (h.id)}
							<tr class:disabled={!h.enabled}>
								<td>
									<code>{h.name}</code>
									{#if !h.enabled}<span class="badge warn">disabled</span>{/if}
									{#if h.description}<div class="muted small">{h.description}</div>{/if}
								</td>
								<td class="small">
									{h.groupIds.map((g) => groupName.get(g) ?? '?').join(', ')}
								</td>
								<td class="small muted"
									>{Object.keys(h.variables).length
										? `${Object.keys(h.variables).length} set`
										: ''}</td
								>
								<td>
									<div class="row-actions">
										<button class="secondary" onclick={() => openHost(h)}>
											{canEditHosts ? 'Edit' : 'View'}
										</button>
										{#if canEditHosts}
											<button class="danger" onclick={() => removeHost(h)}>Delete</button>
										{/if}
									</div>
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
			{#if hostTotal > hosts.length}
				<p class="muted">Showing {hosts.length} of {hostTotal}. Narrow the search to see others.</p>
			{/if}
		{/if}
		{#if hostOpen}
			{#key hostOpen === 'new' ? 'new' : hostOpen.id}
				<HostForm
					inventoryId={inventory.id}
					host={hostOpen === 'new' ? null : hostOpen}
					{groups}
					readonly={!canEditHosts}
					onsaved={hostSaved}
					oncancel={() => (hostOpen = null)}
				/>
			{/key}
		{/if}
	</section>

	<section class="card">
		<div class="page-header">
			<h2>Groups</h2>
			<span class="spacer"></span>
			{#if canEditHosts}<button onclick={() => openGroup('new')}>Add group</button>{/if}
		</div>
		{#if groupError}<p class="error" role="alert">{groupError}</p>{/if}
		{#if groups.length === 0}
			<p class="muted">No groups yet. Every host is in Ansible's <code>all</code> group.</p>
		{:else}
			<div class="table-wrap">
				<table>
					<thead>
						<tr>
							<th>Group</th>
							<th>Children</th>
							<th>Member of</th>
							<th></th>
						</tr>
					</thead>
					<tbody>
						{#each groups as g (g.id)}
							<tr>
								<td>
									<code>{g.name}</code>
									{#if g.description}<div class="muted small">{g.description}</div>{/if}
								</td>
								<td class="small">
									{g.childIds.map((c) => groupName.get(c) ?? '?').join(', ')}
								</td>
								<td class="small">{parentsOf(g).join(', ')}</td>
								<td>
									<div class="row-actions">
										<button class="secondary" onclick={() => (hostGroup = g.id)}>Hosts</button>
										<button class="secondary" onclick={() => openGroup(g)}>
											{canEditHosts ? 'Edit' : 'View'}
										</button>
										{#if canEditHosts}
											<button class="danger" onclick={() => removeGroup(g)}>Delete</button>
										{/if}
									</div>
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
		{#if groupOpen}
			{#key groupOpen === 'new' ? 'new' : groupOpen.id}
				<GroupForm
					inventoryId={inventory.id}
					group={groupOpen === 'new' ? null : groupOpen}
					{groups}
					readonly={!canEditHosts}
					onsaved={groupSaved}
					oncancel={() => (groupOpen = null)}
				/>
			{/key}
		{/if}
	</section>

	<UsedBy {templates} what="inventory" />
	<ResourceAccess type="inventory" id={inventory.id} />
{:else if error}
	<p class="error" role="alert">{error}</p>
	<p><a href="/inventories">Back to inventories</a></p>
{:else}
	<p class="muted">Loading…</p>
{/if}

<style>
	.page-header h2 {
		margin: 0;
	}

	section .page-header {
		margin-bottom: 0.75rem;
	}

	.filters {
		display: flex;
		flex-wrap: wrap;
		gap: 0 1rem;
	}

	.filters label {
		width: 260px;
	}

	tr.disabled code {
		color: var(--muted);
	}
</style>
