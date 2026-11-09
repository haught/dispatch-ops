<script lang="ts">
	import type { NotificationStats } from '#lib/api/client.js';
	import { fmtRelative } from '#lib/format.js';

	/** Notification deliveries in the window by kind of target, and the latest that failed. */
	let { stats, now, period }: { stats: NotificationStats; now: number; period: string } = $props();

	const kindLabel: Record<string, string> = {
		webhook: 'Webhook',
		slack: 'Slack',
		googlechat: 'Google Chat',
		email: 'Email'
	};
</script>

<section class="card">
	<div class="head">
		<h2>Notifications</h2>
		<a href="/notifications" class="small">Targets →</a>
	</div>
	{#if stats.byKind.length === 0}
		<p class="muted">No notifications sent in the {period}.</p>
	{:else}
		<table class="small">
			<thead>
				<tr>
					<th>Kind</th>
					<th class="num">Sent</th>
					<th class="num">Failed</th>
					<th class="num">Pending</th>
				</tr>
			</thead>
			<tbody>
				{#each stats.byKind as k (k.kind)}
					<tr>
						<td>{kindLabel[k.kind] ?? k.kind}</td>
						<td class="num">{k.sent}</td>
						<td class="num" class:bad={k.failed > 0}>{k.failed}</td>
						<td class="num">{k.pending}</td>
					</tr>
				{/each}
			</tbody>
		</table>
		{#if stats.recentFailures.length}
			<h3 class="small">Latest failures</h3>
			<ul>
				{#each stats.recentFailures as f (f.targetId + f.jobId + f.createdAt)}
					<li class="small">
						<strong>{f.targetName}</strong>
						<span class="muted"
							>· {f.event} · <a href="/jobs/{f.jobId}">job</a> · {fmtRelative(
								f.createdAt,
								now
							)}</span
						>
						<div class="reason muted" title={f.error}>{f.error}</div>
					</li>
				{/each}
			</ul>
		{/if}
	{/if}
</section>

<style>
	.head {
		display: flex;
		justify-content: space-between;
		align-items: baseline;
	}

	td,
	th {
		padding: 0.3rem 0.4rem;
	}

	.num {
		text-align: right;
		font-variant-numeric: tabular-nums;
	}

	.bad {
		color: var(--danger);
		font-weight: 600;
	}

	h3 {
		margin: 0.75rem 0 0.25rem;
		color: var(--muted);
		font-weight: 600;
	}

	ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	li {
		padding: 0.25rem 0;
	}

	.reason {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
</style>
