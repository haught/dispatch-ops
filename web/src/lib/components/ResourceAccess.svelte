<script lang="ts">
	import {
		api,
		errorMessage,
		resourceTypes,
		type ResourceType,
		type RoleGrant
	} from '#lib/api/client.js';
	import { can } from '#lib/permissions.svelte.js';
	import { session } from '#lib/session.svelte.js';
	import GrantForm from './GrantForm.svelte';
	import GrantTable from './GrantTable.svelte';

	/** Who holds roles on one resource; its admins can grant and revoke them here. */
	let { type, id }: { type: ResourceType; id: string } = $props();

	let grants = $state<RoleGrant[]>([]);
	let error = $state('');

	const canManage = $derived(can(type, id, 'admin'));
	// The API lists a resource's grants to its admins, and to auditors.
	const canSee = $derived(canManage || Boolean(session.user?.isAuditor));
	const plural = $derived(resourceTypes.find((r) => r.type === type)?.plural ?? type);

	async function load() {
		const { data, error: err } = await api.GET('/api/v1/role-grants', {
			params: { query: { resourceType: type, resourceId: id, limit: 500 } }
		});
		if (data) grants = data.items;
		else error = errorMessage(err);
	}

	$effect(() => {
		void id;
		if (canSee) load();
	});

	async function revoke(g: RoleGrant) {
		error = '';
		const { error: err } = await api.DELETE('/api/v1/role-grants/{grantId}', {
			params: { path: { grantId: g.id } }
		});
		if (err) error = errorMessage(err);
		await load();
	}
</script>

{#if canSee}
	<section class="card">
		<h2>Access</h2>
		{#if error}<p class="error" role="alert">{error}</p>{/if}
		{#if grants.length === 0}
			<p class="muted">No roles granted on this one specifically.</p>
		{:else}
			<GrantTable {grants} onrevoke={canManage ? revoke : undefined} />
		{/if}
		<p class="muted small">
			Roles granted on all {plural}, and system administrators and auditors, apply too.
		</p>
		{#if canManage}
			<h3>Grant a role</h3>
			<GrantForm resourceType={type} resourceId={id} ongranted={load} />
		{/if}
	</section>
{/if}

<style>
	h3 {
		font-size: 1rem;
		margin: 1.25rem 0 0.5rem;
	}
</style>
