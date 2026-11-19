<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { tick } from 'svelte';
	import {
		api,
		errorMessage,
		finishedStatuses,
		hostEventTypes,
		type Job,
		type JobEvent,
		type JobHostCounts,
		type JobHostSummary,
		type NotificationDelivery
	} from '#lib/api/client.js';
	import { parseAnsi } from '#lib/ansi.js';
	import StatusBadge from '#lib/components/StatusBadge.svelte';
	import TabPanel from '#lib/components/TabPanel.svelte';
	import Tabs from '#lib/components/Tabs.svelte';
	import { fmtDuration, fmtTime } from '#lib/format.js';
	import { activeTab, type Tab } from '#lib/tabs.svelte.js';

	let job = $state<Job | null>(null);
	// Raw: the log can be long, and lines never change once added.
	let lines = $state.raw<JobEvent[]>([]);
	let live = $state(false);
	let error = $state('');
	let canceling = $state(false);
	let relaunching = $state(false);
	let deliveries = $state<NotificationDelivery[]>([]);

	// Per-host results (Ansible jobs), and the task results of the host
	// picked in the table.
	let hosts = $state<JobHostSummary[]>([]);
	let hostCounts = $state<JobHostCounts | null>(null);
	let selectedHost = $state<string | null>(null);
	let hostEvents = $state<JobEvent[]>([]);
	let hostEventsError = $state('');

	let logEl = $state<HTMLElement | null>(null);
	let follow = $state(true);
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
		lines = [];
		error = '';
		hosts = [];
		hostCounts = null;
		deliveries = [];
		selectedHost = null;
		let source: EventSource | null = null;
		let pending: JobEvent[] = [];
		let frame = 0;
		let closed = false;
		let hostsTimer: ReturnType<typeof setTimeout> | undefined;

		const loadHosts = async () => {
			const { data } = await api.GET('/api/v1/jobs/{jobId}/hosts', {
				params: { path: { jobId: id } }
			});
			if (data && !closed) {
				hosts = data.items;
				hostCounts = data.counts;
			}
		};
		// Host results change with every task; refresh them at most every 1.5s.
		const scheduleHosts = () => {
			hostsTimer ??= setTimeout(() => {
				hostsTimer = undefined;
				loadHosts();
				if (selectedHost) loadHostEvents(id, selectedHost);
			}, 1500);
		};

		// Batch incoming lines into one update per animation frame.
		const flush = () => {
			frame = 0;
			lines = lines.concat(pending);
			pending = [];
			scrollIfFollowing();
		};

		api
			.GET('/api/v1/jobs/{jobId}', { params: { path: { jobId: id } } })
			.then(({ data, error: err }) => {
				if (closed) return;
				if (!data) {
					error = errorMessage(err);
					return;
				}
				job = data;
				if (data.config.jobType === 'ansible') loadHosts();
				source = new EventSource(`/api/v1/jobs/${id}/stream`);
				live = true;
				source.addEventListener('status', (e) => {
					job = JSON.parse(e.data);
				});
				source.addEventListener('job_event', (e) => {
					const ev: JobEvent = JSON.parse(e.data);
					// The log: output lines, and what Ansible printed for its events.
					if (ev.type === 'stdout' || ev.type === 'runner' || ev.stdout !== '') {
						pending.push(ev);
						if (!frame) frame = requestAnimationFrame(flush);
					}
					if (hostEventTypes.includes(ev.type)) scheduleHosts();
				});
				source.addEventListener('end', () => {
					source?.close();
					live = false;
					if (job?.config.jobType === 'ansible') {
						clearTimeout(hostsTimer);
						hostsTimer = undefined;
						loadHosts();
						if (selectedHost) loadHostEvents(id, selectedHost);
					}
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
			});

		const timer = setInterval(() => (now = Date.now()), 1000);
		return () => {
			closed = true;
			source?.close();
			if (frame) cancelAnimationFrame(frame);
			clearTimeout(hostsTimer);
			clearInterval(timer);
		};
	});

	async function loadHostEvents(jobId: string, host: string) {
		const { data, error: err } = await api.GET('/api/v1/jobs/{jobId}/events', {
			params: { path: { jobId }, query: { hostName: host, limit: 5000 } }
		});
		if (selectedHost !== host) return;
		if (data) {
			hostEvents = data.items.filter((e) => hostEventTypes.includes(e.type));
			hostEventsError = '';
		} else {
			hostEventsError = errorMessage(err);
		}
	}

	function selectHost(host: string) {
		if (!job) return;
		if (selectedHost === host) {
			selectedHost = null;
			return;
		}
		selectedHost = host;
		hostEvents = [];
		hostEventsError = '';
		loadHostEvents(job.id, host);
	}

	/** A task result's outcome, for its badge. */
	function outcome(e: JobEvent): JobHostSummary['status'] {
		switch (e.type) {
			case 'host_ok':
				return e.data?.changed ? 'changed' : 'ok';
			case 'host_failed':
				return 'failed';
			case 'host_unreachable':
				return 'unreachable';
			default:
				return 'skipped';
		}
	}

	const hostExtras = $derived(hosts.some((h) => h.rescued || h.ignored));

	async function scrollIfFollowing() {
		if (!follow) return;
		await tick();
		logEl?.scrollTo({ top: logEl.scrollHeight });
	}

	// The log can't scroll while its tab is hidden; catch up when it shows.
	$effect(() => {
		if (tab === 'output') scrollIfFollowing();
	});

	function onScroll() {
		if (!logEl || tab !== 'output') return;
		follow = logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 40;
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
		<section class="card log-card">
			<pre class="log" bind:this={logEl} onscroll={onScroll}>{#each lines as line (line.seq)}<span
						class="line {line.type}"
						>{#each parseAnsi(line.stdout) as seg, i (i)}<span class={seg.cls}>{seg.text}</span
							>{/each}</span
					>{'\n'}{:else}<span class="muted">{finished ? 'No output.' : 'Waiting for output…'}</span
					>{/each}</pre>
			{#if !follow && live}
				<button class="secondary jump" onclick={() => ((follow = true), scrollIfFollowing())}>
					Follow output ↓
				</button>
			{/if}
		</section>
	</TabPanel>

	<TabPanel id="hosts" active={tab}>
		{#if job.config.jobType === 'ansible'}
			<section class="card">
				<h2>Hosts</h2>
				{#if hosts.length === 0}
					<p class="muted">{finished ? 'No host results.' : 'No host results yet.'}</p>
				{:else}
					<div class="table-wrap">
						<table>
							<thead>
								<tr>
									<th>Host</th>
									<th>Result</th>
									<th class="num">OK</th>
									<th class="num">Changed</th>
									<th class="num">Failed</th>
									<th class="num">Unreachable</th>
									<th class="num">Skipped</th>
									{#if hostExtras}
										<th class="num">Rescued</th>
										<th class="num">Ignored</th>
									{/if}
								</tr>
							</thead>
							<tbody>
								{#each hosts as h (h.hostName)}
									<tr class:selected={selectedHost === h.hostName}>
										<td>
											<button
												class="link"
												aria-expanded={selectedHost === h.hostName}
												onclick={() => selectHost(h.hostName)}>{h.hostName}</button
											>
										</td>
										<td><StatusBadge status={h.status} /></td>
										<td class="num">{h.ok}</td>
										<td class="num">{h.changed}</td>
										<td class="num">{h.failed}</td>
										<td class="num">{h.unreachable}</td>
										<td class="num">{h.skipped}</td>
										{#if hostExtras}
											<td class="num">{h.rescued}</td>
											<td class="num">{h.ignored}</td>
										{/if}
									</tr>
									{#if selectedHost === h.hostName}
										<tr class="host-detail">
											<td colspan={hostExtras ? 9 : 7}>
												{#if hostEventsError}
													<p class="error">{hostEventsError}</p>
												{:else if hostEvents.length === 0}
													<p class="muted">No task results.</p>
												{:else}
													<table class="tasks">
														<thead>
															<tr>
																<th>Task</th>
																<th>Result</th>
																<th>Message</th>
															</tr>
														</thead>
														<tbody>
															{#each hostEvents as e (e.seq)}
																<tr>
																	<td>
																		{e.task?.name ?? ''}
																		{#if e.task?.play}<div class="muted small">
																				{e.task.play}
																			</div>{/if}
																	</td>
																	<td>
																		<StatusBadge status={outcome(e)} />
																		{#if e.data?.ignored}<span class="muted small">ignored</span
																			>{/if}
																	</td>
																	<td class="msg"
																		>{#each parseAnsi(String(e.data?.msg ?? e.data?.stderr ?? '')) as seg, i (i)}<span
																				class={seg.cls}>{seg.text}</span
																			>{/each}</td
																	>
																</tr>
															{/each}
														</tbody>
													</table>
												{/if}
											</td>
										</tr>
									{/if}
								{/each}
							</tbody>
						</table>
					</div>
				{/if}
			</section>
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

	.table-wrap {
		overflow-x: auto;
	}

	.num {
		text-align: right;
		font-variant-numeric: tabular-nums;
	}

	button.link {
		padding: 0;
		border: none;
		background: none;
		color: var(--accent);
		text-align: left;
	}

	tr.selected > td {
		background: var(--bg);
	}

	.host-detail > td {
		background: var(--bg);
		padding: 0.25rem 0.5rem 0.75rem 1.5rem;
	}

	.tasks th,
	.tasks td {
		font-size: 0.9rem;
		vertical-align: top;
	}

	.msg {
		white-space: pre-wrap;
		overflow-wrap: anywhere;
		font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
		font-size: 0.85rem;
	}

	.small {
		font-size: 0.8rem;
	}

	.log-card {
		position: relative;
		padding: 0;
	}

	.log {
		margin: 0;
		padding: 1rem;
		height: 60vh;
		overflow: auto;
		font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
		font-size: 0.85rem;
		line-height: 1.45;
		white-space: pre-wrap;
		overflow-wrap: anywhere;
	}

	.line.runner {
		color: var(--muted);
		font-style: italic;
	}

	.jump {
		position: absolute;
		right: 1rem;
		bottom: 1rem;
		background: var(--surface);
	}
</style>
