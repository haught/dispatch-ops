<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import {
		api,
		errorMessage,
		finishedStatuses,
		type Job,
		type JobEvent,
		type JobHostCounts,
		type NotificationDelivery
	} from '#lib/api/client.js';
	import JobHosts from '#lib/components/JobHosts.svelte';
	import JobLog from '#lib/components/JobLog.svelte';
	import StatusBadge from '#lib/components/StatusBadge.svelte';
	import TabPanel from '#lib/components/TabPanel.svelte';
	import Tabs from '#lib/components/Tabs.svelte';
	import { fmtDuration, fmtTime } from '#lib/format.js';
	import { JobLog as LogBuffer, PAGE } from '#lib/joblog.svelte.js';
	import { activeTab, type Tab } from '#lib/tabs.svelte.js';

	let job = $state<Job | null>(null);
	const log = new LogBuffer();
	let live = $state(false);
	let error = $state('');
	let canceling = $state(false);
	let relaunching = $state(false);
	let deliveries = $state<NotificationDelivery[]>([]);

	// Per-host results (Ansible jobs): their counts, and a counter bumped
	// whenever they change.
	let hostCounts = $state<JobHostCounts | null>(null);
	let hostsChanged = $state(0);

	// Ticks once a second so the running duration updates.
	let now = $state(Date.now());

	const finished = $derived(job ? finishedStatuses.includes(job.status) : false);
	const failedHosts = $derived(hostCounts ? hostCounts.failed + hostCounts.unreachable : 0);
	const hostTotal = $derived(
		hostCounts ? Object.values(hostCounts).reduce((sum, n) => sum + n, 0) : 0
	);

	const tabs = $derived<Tab[]>([
		{ id: 'output', label: 'Output' },
		...(job?.config.jobType === 'ansible'
			? [{ id: 'hosts', label: 'Hosts', count: hostTotal || undefined }]
			: []),
		{ id: 'details', label: 'Details' },
		...(deliveries.length > 0
			? [{ id: 'notifications', label: 'Notifications', count: deliveries.length }]
			: [])
	]);
	const tab = $derived(activeTab(tabs));

	async function loadDeliveries(id: string) {
		const { data } = await api.GET('/api/v1/jobs/{jobId}/notifications', {
			params: { path: { jobId: id } }
		});
		if (data && page.params.id === id) deliveries = data.items;
	}

	// Notifications are queued as the status changes and sent shortly after.
	$effect(() => {
		const id = job?.id;
		void job?.status;
		if (!id) return;
		loadDeliveries(id);
		const timer = setTimeout(() => loadDeliveries(id), 3000);
		return () => clearTimeout(timer);
	});

	$effect(() => {
		const id = page.params.id;
		if (!id) return;
		job = null;
		log.clear();
		error = '';
		hostCounts = null;
		deliveries = [];
		let source: EventSource | null = null;
		let pending: JobEvent[] = [];
		let frame = 0;
		let closed = false;

		// Batch incoming lines into one update per animation frame.
		const flush = () => {
			frame = 0;
			log.append(pending);
			pending = [];
		};

		(async () => {
			const { data, error: err } = await api.GET('/api/v1/jobs/{jobId}', {
				params: { path: { jobId: id } }
			});
			if (closed) return;
			if (!data) {
				error = errorMessage(err);
				return;
			}
			job = data;

			// The end of the log, then what follows, live.
			const tail = await api.GET('/api/v1/jobs/{jobId}/events', {
				params: {
					path: { jobId: id },
					query: { log: true, beforeSeq: Number.MAX_SAFE_INTEGER, limit: PAGE }
				}
			});
			if (closed) return;
			if (!tail.data) {
				error = errorMessage(tail.error);
				return;
			}
			log.prepend(tail.data.items, tail.data.items.length === PAGE);

			source = new EventSource(`/api/v1/jobs/${id}/stream?afterSeq=${log.lastSeq}`);
			live = true;
			source.addEventListener('status', (e) => {
				job = JSON.parse(e.data);
			});
			source.addEventListener('job_event', (e) => {
				pending.push(JSON.parse(e.data));
				if (!frame) frame = requestAnimationFrame(flush);
			});
			source.addEventListener('hosts', () => hostsChanged++);
			source.addEventListener('end', () => {
				source?.close();
				live = false;
				hostsChanged++;
			});
			source.addEventListener('revoked', () => {
				// Don't let the browser reconnect: access has ended.
				source?.close();
				live = false;
				error = 'You no longer have access to this job. Sign in again or ask an admin.';
			});
			source.onerror = () => {
				// The browser reconnects by itself (resuming with Last-Event-ID)
				// unless the server refused the stream.
				if (source?.readyState === EventSource.CLOSED) {
					live = false;
					error = 'Lost the live connection. Reload to try again.';
				}
			};
		})();

		const timer = setInterval(() => (now = Date.now()), 1000);
		return () => {
			closed = true;
			source?.close();
			if (frame) cancelAnimationFrame(frame);
			clearInterval(timer);
		};
	});

	async function loadEarlier() {
		if (!job) return;
		const { data, error: err } = await api.GET('/api/v1/jobs/{jobId}/events', {
			params: {
				path: { jobId: job.id },
				query: { log: true, beforeSeq: log.firstSeq, limit: PAGE }
			}
		});
		if (data) log.prepend(data.items, data.items.length === PAGE);
		else error = errorMessage(err);
	}

	async function cancel() {
		if (!job) return;
		canceling = true;
		const { data, error: err } = await api.POST('/api/v1/jobs/{jobId}/cancel', {
			params: { path: { jobId: job.id } }
		});
		canceling = false;
		if (data) job = data;
		else error = errorMessage(err);
	}

	async function relaunch(hosts: 'all' | 'failed') {
		if (!job) return;
		relaunching = true;
		error = '';
		const { data, error: err } = await api.POST('/api/v1/jobs/{jobId}/relaunch', {
			params: { path: { jobId: job.id } },
			body: { hosts }
		});
		relaunching = false;
		if (data) goto(`/jobs/${data.id}`);
		else error = errorMessage(err);
	}

	function duration(j: Job) {
		void now;
		return fmtDuration(j.startedAt, j.finishedAt);
	}
</script>

{#if job}
	<div class="header">
		<h1>{job.name}</h1>
		<StatusBadge status={job.status} />
		{#if live}<span class="live" title="Receiving output live">● live</span>{/if}
		<span class="spacer"></span>
		{#if !finished}
			<button class="danger" onclick={cancel} disabled={canceling || job.cancelRequested}>
				{job.cancelRequested ? 'Canceling…' : 'Cancel'}
			</button>
		{:else}
			{#if job.config.jobType === 'ansible' && failedHosts > 0}
				<button class="secondary" onclick={() => relaunch('failed')} disabled={relaunching}>
					Relaunch on failed hosts ({failedHosts})
				</button>
			{/if}
			<button class="secondary" onclick={() => relaunch('all')} disabled={relaunching}>
				{relaunching ? 'Relaunching…' : 'Relaunch'}
			</button>
		{/if}
	</div>

	{#if job.failureReason}
		<p class="reason" class:error={job.status !== 'canceled'}>{job.failureReason}</p>
	{/if}
	{#if error}
		<p class="error" role="alert">{error}</p>
	{/if}

	<p class="summary muted">
		{#if job.jobTemplateId}
			<span>Template <a href="/templates/{job.jobTemplateId}">{job.name}</a></span>
		{/if}
		<span>
			{#if job.schedule}
				Launched by schedule {job.schedule.name}
			{:else}
				Launched by {job.launchedBy?.username ?? '—'} ({job.launchType})
			{/if}
		</span>
		{#if job.startedAt}<span>Started {fmtTime(job.startedAt)}</span>{/if}
		<span>Duration {duration(job)}</span>
	</p>

	<Tabs {tabs} label="Job sections" />

	<TabPanel id="output" active={tab}>
		{#if job.outputTruncated}
			<p class="muted small">
				The output reached its size limit, so its end was not kept. Host results are complete.
			</p>
		{/if}
		<JobLog {log} jobId={job.id} {live} {finished} visible={tab === 'output'} {loadEarlier} />
	</TabPanel>

	<TabPanel id="hosts" active={tab}>
		{#if job.config.jobType === 'ansible'}
			<JobHosts
				jobId={job.id}
				{finished}
				visible={tab === 'hosts'}
				changed={hostsChanged}
				bind:counts={hostCounts}
			/>
		{/if}
	</TabPanel>

	<TabPanel id="details" active={tab}>
		<section class="card">
			<dl>
				<dt>Type</dt>
				<dd>{job.config.jobType}</dd>
				{#if job.config.project}
					<dt>Project</dt>
					<dd>
						{job.config.project.name}
						<span class="muted">
							(<code>{job.config.project.scmUrl}</code> at <code>{job.config.project.scmRef}</code>)
						</span>
					</dd>
					<dt>
						{job.config.jobType === 'ansible'
							? 'Playbook'
							: job.config.jobType === 'inventory_sync'
								? 'Inventory source'
								: 'Script'}
					</dt>
					<dd><code>{[job.config.entrypoint, ...job.config.arguments].join(' ')}</code></dd>
					<dt>Revision</dt>
					<dd>
						{#if job.scmRevision}<code title={job.scmRevision}>{job.scmRevision.slice(0, 12)}</code
							>{:else}—{/if}
					</dd>
				{:else}
					<dt>Command</dt>
					<dd><code>{job.config.command.join(' ')}</code></dd>
				{/if}
				<dt>Image</dt>
				<dd><code>{job.config.executionEnvironment.image}</code></dd>
				{#if job.config.limit}
					<dt>Limit</dt>
					<dd><code>{job.config.limit}</code></dd>
				{/if}
				{#if job.config.tags}
					<dt>Tags</dt>
					<dd><code>{job.config.tags}</code></dd>
				{/if}
				{#if job.config.skipTags}
					<dt>Skip tags</dt>
					<dd><code>{job.config.skipTags}</code></dd>
				{/if}
				<dt>Launched by</dt>
				<dd>
					{#if job.schedule}
						schedule <strong>{job.schedule.name}</strong>
					{:else}
						{job.launchedBy?.username ?? '—'} ({job.launchType})
					{/if}
					{#if job.relaunchOf}
						<span class="muted">· rerun of <a href="/jobs/{job.relaunchOf}">an earlier job</a></span
						>
					{/if}
				</dd>
				{#if job.jobTemplateId}
					<dt>Template</dt>
					<dd><a href="/templates/{job.jobTemplateId}">{job.name}</a></dd>
				{/if}
				{#if job.syncInventoryId}
					<dt>Syncs</dt>
					<dd><a href="/inventories/{job.syncInventoryId}">the inventory</a></dd>
				{/if}
				<dt>Created</dt>
				<dd>{fmtTime(job.createdAt)}</dd>
				<dt>Started</dt>
				<dd>{fmtTime(job.startedAt)}</dd>
				<dt>Duration</dt>
				<dd>{duration(job)}</dd>
				{#if job.exitCode !== null}
					<dt>Exit code</dt>
					<dd>{job.exitCode}</dd>
				{/if}
			</dl>
		</section>
	</TabPanel>

	<TabPanel id="notifications" active={tab}>
		{#if deliveries.length > 0}
			<section class="card">
				<h2>Notifications</h2>
				<div class="table-wrap">
					<table>
						<thead>
							<tr>
								<th>Target</th>
								<th>Event</th>
								<th>Status</th>
								<th>Details</th>
							</tr>
						</thead>
						<tbody>
							{#each deliveries as d (d.id)}
								<tr>
									<td>{d.targetName} <span class="muted small">{d.targetKind}</span></td>
									<td>{d.event}</td>
									<td>
										<span
											class="badge"
											class:accent={d.status === 'sent'}
											class:warn={d.status !== 'sent'}>{d.status}</span
										>
									</td>
									<td class="small">
										{#if d.status === 'sent'}
											{fmtTime(d.sentAt)}
										{:else if d.status === 'pending' && d.attempts > 0}
											Attempt {d.attempts} failed: {d.lastError}. Retrying at {fmtTime(
												d.nextAttemptAt
											)}.
										{:else if d.status === 'failed'}
											<span class="error">{d.lastError}</span>
										{:else}
											<span class="muted">Sending…</span>
										{/if}
									</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			</section>
		{/if}
	</TabPanel>
{:else if error}
	<p class="error" role="alert">{error}</p>
	<p><a href="/jobs">Back to jobs</a></p>
{:else}
	<p class="muted">Loading…</p>
{/if}

<style>
	.header {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		margin-bottom: 0.5rem;
	}

	.header h1 {
		margin: 0;
	}

	.spacer {
		flex: 1;
	}

	.live {
		color: var(--ok);
		font-size: 0.85rem;
	}

	.reason {
		margin-top: 0;
	}

	.summary {
		display: flex;
		flex-wrap: wrap;
		gap: 0.25rem 1.5rem;
		margin: 0;
		font-size: 0.9rem;
	}

	dl {
		display: grid;
		grid-template-columns: max-content 1fr;
		gap: 0.25rem 1rem;
		margin: 0;
	}

	dt {
		color: var(--muted);
	}

	dd {
		margin: 0;
		overflow-wrap: anywhere;
	}

	.small {
		font-size: 0.8rem;
	}
</style>
