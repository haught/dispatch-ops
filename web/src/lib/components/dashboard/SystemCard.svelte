<script lang="ts">
	import type { Health, QueueStats } from '#lib/api/client.js';
	import { fmtSeconds } from '#lib/format.js';

	/** Service health, plus the job and notification queues for those who may see them. */
	let { health, queue }: { health: Health | null; queue: QueueStats | null | undefined } = $props();

	// A job waiting this long for a pod suggests the task manager or cluster is stuck.
	const slowQueueSeconds = 300;
</script>

<section class="card">
	<h2>System</h2>
	{#if health}
		<dl>
			<dt>Status</dt>
			<dd class:ok={health.status === 'ok'} class:error={health.status !== 'ok'}>
				{health.status === 'ok' ? '● ok' : '▲ ' + health.status}
			</dd>
			<dt>Database</dt>
			<dd class:ok={health.database === 'ok'} class:error={health.database !== 'ok'}>
				{health.database === 'ok' ? '● ok' : '▲ ' + health.database}
			</dd>
			{#if queue}
				<dt>Job queue</dt>
				<dd>
					{queue.pending} pending · {queue.waiting} starting · {queue.running} running
				</dd>
				<dt>Oldest pending</dt>
				<dd class:warn={queue.oldestPendingSeconds > slowQueueSeconds}>
					{queue.pending ? fmtSeconds(queue.oldestPendingSeconds) : '—'}
					{#if queue.oldestPendingSeconds > slowQueueSeconds}<span class="small">(slow)</span>{/if}
				</dd>
				<dt>Notifications</dt>
				<dd class:warn={queue.notificationsPending > 0}>
					{queue.notificationsPending} waiting to send
				</dd>
			{/if}
			<dt>Version</dt>
			<dd>{health.version ?? 'unknown'}</dd>
		</dl>
	{:else}
		<p class="muted">Checking…</p>
	{/if}
</section>

<style>
	dl {
		display: grid;
		grid-template-columns: max-content 1fr;
		gap: 0.3rem 1rem;
		margin: 0;
	}

	dt {
		color: var(--muted);
	}

	dd {
		margin: 0;
		font-variant-numeric: tabular-nums;
	}

	.ok {
		color: var(--ok);
	}

	.warn {
		color: var(--warn);
	}
</style>
