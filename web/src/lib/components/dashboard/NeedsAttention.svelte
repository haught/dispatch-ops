<script lang="ts">
	import { goto } from '$app/navigation';
	import { api, errorMessage, type Job } from '#lib/api/client.js';
	import StatusBadge from '#lib/components/StatusBadge.svelte';
	import { fmtRelative } from '#lib/format.js';
	import { can } from '#lib/permissions.svelte.js';

	/** Recent jobs that failed or errored, with why and a way to rerun them. */
	let { jobs, now, period }: { jobs: Job[]; now: number; period: string } = $props();

	let relaunching = $state<string | null>(null);
	let error = $state('');

	async function relaunch(job: Job) {
		relaunching = job.id;
		error = '';
		const { data, error: err } = await api.POST('/api/v1/jobs/{jobId}/relaunch', {
			params: { path: { jobId: job.id } },
			body: { hosts: 'all' }
		});
		relaunching = null;
		if (data) await goto(`/jobs/${data.id}`);
		else error = errorMessage(err);
	}
</script>

<section class="card">
	<h2>Needs attention</h2>
	{#if error}
		<p class="error small" role="alert">{error}</p>
	{/if}
	{#if jobs.length === 0}
		<p class="muted">No failed jobs in the {period}.</p>
	{:else}
		<ul>
			{#each jobs as job (job.id)}
				<li>
					<div class="line">
						<a href="/jobs/{job.id}" class="name">{job.name}</a>
						<StatusBadge status={job.status} />
						<span class="muted small nowrap"
							>{fmtRelative(job.finishedAt ?? job.createdAt, now)}</span
						>
						{#if job.jobTemplateId && can('job_template', job.jobTemplateId, 'execute')}
							<button
								class="secondary small"
								disabled={relaunching !== null}
								onclick={() => relaunch(job)}
							>
								{relaunching === job.id ? 'Relaunching…' : 'Relaunch'}
							</button>
						{/if}
					</div>
					{#if job.failureReason}
						<div class="reason muted small" title={job.failureReason}>{job.failureReason}</div>
					{/if}
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
		padding: 0.4rem 0;
		border-bottom: 1px solid var(--border);
	}

	li:last-child {
		border-bottom: none;
	}

	.line {
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}

	.name {
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.nowrap {
		white-space: nowrap;
	}

	button.small {
		padding: 0.15rem 0.5rem;
	}

	.reason {
		margin-top: 0.15rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
</style>
