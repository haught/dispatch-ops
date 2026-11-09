<script lang="ts">
	import type { InventorySyncHealth } from '#lib/api/client.js';
	import StatusBadge from '#lib/components/StatusBadge.svelte';
	import { fmtRelative } from '#lib/format.js';

	/** Inventories that sync from a project: when they last synced, and whether that worked. */
	let { syncs, now }: { syncs: InventorySyncHealth[]; now: number } = $props();

	const shown = 6;
	const failing = $derived(
		syncs.filter((s) => s.lastJobStatus === 'failed' || s.lastJobStatus === 'error').length
	);
</script>

<section class="card">
	<h2>Inventory syncs</h2>
	{#if syncs.length === 0}
		<p class="muted">
			No inventory syncs from a project. Set a source on an <a href="/inventories">inventory</a> to keep
			its hosts in sync.
		</p>
	{:else}
		{#if failing}
			<p class="small error">▲ {failing} of {syncs.length} failed their last sync</p>
		{/if}
		<ul>
			{#each syncs.slice(0, shown) as s (s.inventoryId)}
				<li>
					<a href="/inventories/{s.inventoryId}" class="name">{s.inventoryName}</a>
					{#if s.lastJobStatus && s.lastJobId}
						<a href="/jobs/{s.lastJobId}"><StatusBadge status={s.lastJobStatus} /></a>
					{/if}
					<span
						class="muted small when"
						title={s.lastSyncedAt ? new Date(s.lastSyncedAt).toLocaleString() : undefined}
					>
						{s.lastSyncedAt ? `synced ${fmtRelative(s.lastSyncedAt, now)}` : 'never synced'}
					</span>
				</li>
			{/each}
		</ul>
		{#if syncs.length > shown}
			<p class="muted small more">and {syncs.length - shown} more, synced more recently</p>
		{/if}
	{/if}
</section>

<style>
	ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	li {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		padding: 0.35rem 0;
		border-bottom: 1px solid var(--border);
	}

	li:last-child {
		border-bottom: none;
	}

	.name {
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.when {
		white-space: nowrap;
	}

	.more {
		margin: 0.25rem 0 0;
	}
</style>
