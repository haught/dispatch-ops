<script lang="ts">
	import type { AuditEntry } from '#lib/api/client.js';
	import { fmtRelative } from '#lib/format.js';

	/** The latest audit log entries: who changed what. */
	let { entries, now }: { entries: AuditEntry[]; now: number } = $props();

	const what = (e: AuditEntry) => e.resourceType.replaceAll('_', ' ');
</script>

<section class="card">
	<div class="head">
		<h2>Recent changes</h2>
		<a href="/audit" class="small">Audit log →</a>
	</div>
	{#if entries.length === 0}
		<p class="muted">No changes recorded yet.</p>
	{:else}
		<ul>
			{#each entries as e (e.id)}
				<li class="small">
					<span>
						<strong>{e.actorName || 'system'}</strong>
						<span class="muted">{e.action}</span>
						{what(e)}
						{#if e.resourceName}<em>{e.resourceName}</em>{/if}
					</span>
					<span class="muted when" title={new Date(e.time).toLocaleString()}
						>{fmtRelative(e.time, now)}</span
					>
				</li>
			{/each}
		</ul>
	{/if}
</section>

<style>
	.head {
		display: flex;
		justify-content: space-between;
		align-items: baseline;
	}

	ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	li {
		display: flex;
		justify-content: space-between;
		gap: 0.75rem;
		padding: 0.3rem 0;
		border-bottom: 1px solid var(--border);
	}

	li:last-child {
		border-bottom: none;
	}

	li > span:first-child {
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	em {
		font-style: normal;
		font-weight: 500;
	}

	.when {
		white-space: nowrap;
	}
</style>
