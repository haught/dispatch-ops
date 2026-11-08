<script lang="ts">
	import { goto } from '$app/navigation';
	import { api, errorMessage, type Inventory } from '#lib/api/client.js';
	import VarsField from '#lib/components/VarsField.svelte';
	import { fmtTime, parseVars, varsError } from '#lib/format.js';
	import { canCreate, loadMyGrants } from '#lib/permissions.svelte.js';

	let inventories = $state<Inventory[]>([]);
	let total = $state(0);
	let search = $state('');
	let loading = $state(true);
	let error = $state('');

	let name = $state('');
	let description = $state('');
	let vars = $state('');
	let formError = $state('');
	let saving = $state(false);

	const mayCreate = $derived(canCreate('inventory'));

	async function load() {
		const { data, error: err } = await api.GET('/api/v1/inventories', {
			params: { query: { limit: 500, search: search || undefined } }
		});
		loading = false;
		if (data) {
			inventories = data.items;
			total = data.total;
		} else {
			error = errorMessage(err);
		}
	}

	$effect(() => {
		void search;
		load();
	});

	$effect(() => {
		loadMyGrants();
	});

	async function create(event: SubmitEvent) {
		event.preventDefault();
		formError = varsError(vars);
		if (formError) return;
		saving = true;
		const { data, error: err } = await api.POST('/api/v1/inventories', {
			body: { name, description, variables: parseVars(vars) }
		});
		saving = false;
		if (data) goto(`/inventories/${data.id}`);
		else formError = errorMessage(err);
	}
</script>

<h1>Inventories</h1>
<p class="muted">
	Hosts and host groups jobs run against, with variables at the inventory, group and host level.
</p>

<label class="search">
	Search
	<input bind:value={search} placeholder="Name contains…" />
</label>

{#if error}
	<p class="error" role="alert">{error}</p>
{/if}

<section class="card">
	{#if loading}
		<p class="muted">Loading…</p>
	{:else if inventories.length === 0}
		<p class="muted">No inventories{search ? ' match' : ' yet'}.</p>
	{:else}
		<div class="table-wrap">
			<table>
				<thead>
					<tr>
						<th>Name</th>
						<th>Hosts</th>
						<th>Groups</th>
						<th>Updated</th>
					</tr>
				</thead>
				<tbody>
					{#each inventories as inv (inv.id)}
						<tr>
							<td>
								<a href="/inventories/{inv.id}">{inv.name}</a>
								{#if inv.description}<div class="muted small">{inv.description}</div>{/if}
							</td>
							<td>{inv.hostCount}</td>
							<td>{inv.groupCount}</td>
							<td class="small">{fmtTime(inv.updatedAt)}</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
		{#if total > inventories.length}
			<p class="muted">Showing {inventories.length} of {total}. Narrow the search to see others.</p>
		{/if}
	{/if}
</section>

{#if mayCreate}
	<section class="card">
		<h2>New inventory</h2>
		{#if formError}<p class="error" role="alert">{formError}</p>{/if}
		<form onsubmit={create}>
			<div class="form-grid">
				<label>Name <input bind:value={name} maxlength="255" required /></label>
				<label>Description <input bind:value={description} maxlength="4096" /></label>
			</div>
			<VarsField bind:value={vars} label="Inventory variables" />
			<div class="form-actions">
				<button type="submit" disabled={saving}>{saving ? 'Creating…' : 'Create inventory'}</button>
				<span class="muted small">Add hosts and groups next.</span>
			</div>
		</form>
	</section>
{/if}

<style>
	.search {
		max-width: 320px;
	}
</style>
