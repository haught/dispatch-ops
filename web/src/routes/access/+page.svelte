<script lang="ts">
	import {
		api,
		errorMessage,
		resourceTypes,
		type ResourceType,
		type RoleGrant
	} from '#lib/api/client.js';
	import GrantForm from '#lib/components/GrantForm.svelte';
	import GrantTable from '#lib/components/GrantTable.svelte';
	import { session } from '#lib/session.svelte.js';

	type Filter = '' | 'global' | ResourceType;

	let grants = $state<RoleGrant[]>([]);
	let total = $state(0);
	let filter = $state<Filter>('');
	let loading = $state(true);
	let error = $state('');

	const isAdmin = $derived(Boolean(session.user?.isAdmin));

	async function load() {
		const { data, error: err } = await api.GET('/api/v1/role-grants', {
			params: { query: { limit: 500, resourceType: filter || undefined } }
		});
		loading = false;
		if (data) {
			grants = data.items;
			total = data.total;
			error = '';
		} else {
			error = errorMessage(err);
		}
	}

	$effect(() => {
		void filter;
		load();
	});

	async function revoke(g: RoleGrant) {
		const { error: err } = await api.DELETE('/api/v1/role-grants/{grantId}', {
			params: { path: { grantId: g.id } }
		});
		if (err) error = errorMessage(err);
		await load();
	}
</script>

<h1>Access</h1>
<p class="muted">
	Who holds which role. Roles are granted to users or teams, on one resource, on every resource of a
	type, or globally: <strong>system_admin</strong> can do everything, and
	<strong>auditor</strong> can read everything, including the audit log.
</p>

{#if error}
	<p class="error" role="alert">{error}</p>
{/if}

{#if isAdmin}
	<section class="card">
		<h2>Grant a role</h2>
		<GrantForm ongranted={load} />
	</section>
{/if}

<section class="card">
	<label class="filter">
		Show
		<select bind:value={filter}>
			<option value="">All roles</option>
			<option value="global">Global roles</option>
			{#each resourceTypes as rt (rt.type)}
				<option value={rt.type}>Roles on {rt.plural}</option>
			{/each}
		</select>
	</label>
	{#if loading}
		<p class="muted">Loading…</p>
	{:else if grants.length === 0}
		<p class="muted">No roles granted.</p>
	{:else}
		<GrantTable {grants} onrevoke={isAdmin ? revoke : undefined} />
		{#if total > grants.length}
			<p class="muted">Showing {grants.length} of {total}.</p>
		{/if}
	{/if}
</section>

<style>
	.filter {
		max-width: 260px;
	}
</style>
