<script lang="ts">
	import type { ProblemHost } from '#lib/api/client.js';
	import { fmtRelative } from '#lib/format.js';

	/** Hosts that failed or were unreachable in the most jobs, with a bar for each count. */
	let { hosts, now, period }: { hosts: ProblemHost[]; now: number; period: string } = $props();

	const max = $derived(Math.max(1, ...hosts.map((h) => h.failedJobs + h.unreachableJobs)));

	const jobs = (n: number) => `${n} job${n === 1 ? '' : 's'}`;
	const summary = (h: ProblemHost) =>
		[
			h.failedJobs && `failed in ${jobs(h.failedJobs)}`,
			h.unreachableJobs && `unreachable in ${jobs(h.unreachableJobs)}`
		]
			.filter(Boolean)
			.join(' · ');
</script>

<section class="card">
	<h2>Problem hosts</h2>
	{#if hosts.length === 0}
		<p class="muted">No host failed or was unreachable in the {period}.</p>
	{:else}
		<ul>
			{#each hosts as h (h.hostName)}
				<li>
					<div class="line">
						<span class="host" title={h.hostName}>{h.hostName}</span>
						<a href="/jobs/{h.lastJobId}" class="muted small" title="The last job it failed in">
							{fmtRelative(h.lastSeenAt, now)}
						</a>
					</div>
					<div class="bars" aria-hidden="true">
						{#if h.failedJobs}
							<span class="bar failed" style:width="{(h.failedJobs / max) * 100}%"></span>
						{/if}
						{#if h.unreachableJobs}
							<span class="bar unreachable" style:width="{(h.unreachableJobs / max) * 100}%"></span>
						{/if}
					</div>
					<div class="small muted">{summary(h)}</div>
				</li>
			{/each}
		</ul>
	{/if}
</section>

<style>
	ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	li {
		padding: 0.35rem 0;
	}

	.line {
		display: flex;
		justify-content: space-between;
		gap: 0.5rem;
	}

	.host {
		font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
		font-size: 0.85rem;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.bars {
		display: flex;
		gap: 2px;
		height: 6px;
		margin: 0.2rem 0 0.1rem;
	}

	.bar {
		display: block;
		height: 100%;
		border-radius: 0 3px 3px 0;
		min-width: 4px;
	}

	.bar.failed {
		background: repeating-linear-gradient(
			45deg,
			var(--danger) 0 3px,
			color-mix(in srgb, var(--danger) 60%, var(--surface)) 3px 5px
		);
	}

	.bar.unreachable {
		background: var(--warn);
	}
</style>
