<script lang="ts">
	import { page } from '$app/state';
	import {
		api,
		errorMessage,
		finishedStatuses,
		type Job,
		type JobStatus
	} from '#lib/api/client.js';
	import StatusBadge from '#lib/components/StatusBadge.svelte';
	import { fmtDuration, fmtTime } from '#lib/format.js';

	const pageSize = 50;
	const statuses: JobStatus[] = [
		'pending',
		'waiting',
		'running',
		'successful',
		'failed',
		'error',
		'canceled'
	];

	let jobs = $state<Job[]>([]);
	let total = $state(0);
	let offset = $state(0);
	let status = $state<JobStatus | ''>('');
	let loading = $state(true);
	let error = $state('');

	// Links from a template or schedule narrow the list to its jobs.
	const jobTemplateId = $derived(page.url.searchParams.get('jobTemplateId') || undefined);
	const scheduleId = $derived(page.url.searchParams.get('scheduleId') || undefined);

	async function load() {
		const { data, error: err } = await api.GET('/api/v1/jobs', {
			params: {
				query: { limit: pageSize, offset, status: status || undefined, jobTemplateId, scheduleId }
			}
		});
		loading = false;
		if (data) {
			jobs = data.items;
			total = data.total;
			error = '';
		} else {
			error = errorMessage(err);
		}
	}

	// Reload when the filter or page changes, and every few seconds while
	// any listed job is still active.
	$effect(() => {
		void status;
		void offset;
		void jobTemplateId;
		void scheduleId;
		load();
		const timer = setInterval(() => {
			if (jobs.some((j) => !finishedStatuses.includes(j.status))) load();
		}, 3000);
		return () => clearInterval(timer);
	});
</script>

<h1>Jobs</h1>

<div class="toolbar">
	<label>
		Status
		<select
			bind:value={status}
			onchange={() => {
				offset = 0;
			}}
		>
			<option value="">All</option>
			{#each statuses as s (s)}
				<option value={s}>{s}</option>
			{/each}
		</select>
	</label>
	{#if jobTemplateId || scheduleId}
		<a href="/jobs">Show all jobs</a>
	{:else}
		<a href="/templates">Launch a job template →</a>
	{/if}
</div>

{#if error}
	<p class="error" role="alert">{error}</p>
{/if}

<section class="card">
	{#if loading}
		<p class="muted">Loading…</p>
	{:else if jobs.length === 0}
		<p class="muted">No jobs yet.</p>
	{:else}
		<div class="table-wrap">
			<table>
				<thead>
					<tr>
						<th>Job</th>
						<th>Status</th>
						<th>Launched by</th>
						<th>Created</th>
						<th>Duration</th>
					</tr>
				</thead>
				<tbody>
					{#each jobs as job (job.id)}
						<tr>
							<td><a href="/jobs/{job.id}">{job.name}</a></td>
							<td><StatusBadge status={job.status} /></td>
							<td>
								{job.schedule ? `schedule ${job.schedule.name}` : (job.launchedBy?.username ?? '—')}
							</td>
							<td>{fmtTime(job.createdAt)}</td>
							<td>{fmtDuration(job.startedAt, job.finishedAt)}</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
		{#if total > pageSize}
			<div class="pager">
				<button
					class="secondary"
					disabled={offset === 0}
					onclick={() => (offset = Math.max(0, offset - pageSize))}>Newer</button
				>
				<span class="muted">{offset + 1}–{Math.min(offset + pageSize, total)} of {total}</span>
				<button
					class="secondary"
					disabled={offset + pageSize >= total}
					onclick={() => (offset += pageSize)}>Older</button
				>
			</div>
		{/if}
	{/if}
</section>

<style>
	.toolbar {
		display: flex;
		justify-content: space-between;
		align-items: end;
		gap: 1rem;
		margin-bottom: 1rem;
	}

	.toolbar label {
		margin: 0;
	}

	select {
		display: block;
		margin-top: 0.25rem;
		font: inherit;
		padding: 0.4rem 0.5rem;
		border: 1px solid var(--border);
		border-radius: 6px;
		background: var(--surface);
		color: var(--text);
	}

	.table-wrap {
		overflow-x: auto;
	}

	.pager {
		display: flex;
		justify-content: flex-end;
		align-items: center;
		gap: 1rem;
		margin-top: 1rem;
	}
</style>
