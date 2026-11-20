<script lang="ts">
	import {
		api,
		errorMessage,
		hostEventTypes,
		type JobEvent,
		type JobHostCounts,
		type JobHostStatus,
		type JobHostSummary
	} from '#lib/api/client.js';
	import { parseAnsi } from '#lib/ansi.js';
	import StatusBadge from '#lib/components/StatusBadge.svelte';

	/**
	 * An Ansible job's per-host results, a page at a time, with each host's
	 * task results on demand. While hidden it only keeps `counts` current.
	 */
	let {
		jobId,
		finished,
		visible,
		changed,
		counts = $bindable(null)
	}: {
		jobId: string;
		finished: boolean;
		visible: boolean;
		/** Changes whenever the job's host results have. */
		changed: number;
		counts?: JobHostCounts | null;
	} = $props();

	const pageSize = 100;
	const eventsPage = 500;
	const statuses: { value: JobHostStatus; label: string }[] = [
		{ value: 'failed', label: 'Failed' },
		{ value: 'unreachable', label: 'Unreachable' },
		{ value: 'changed', label: 'Changed' },
		{ value: 'ok', label: 'OK' },
		{ value: 'skipped', label: 'Skipped' }
	];

	let hosts = $state<JobHostSummary[]>([]);
	let total = $state(0);
	let status = $state<JobHostStatus | ''>('');
	let search = $state('');
	let query = $state('');
	let offset = $state(0);
	let error = $state('');

	let selected = $state<string | null>(null);
	let events = $state<JobEvent[]>([]);
	let eventsMore = $state(false);
	let eventsError = $state('');

	const all = $derived(counts ? Object.values(counts).reduce((sum, n) => sum + n, 0) : 0);
	const extras = $derived(hosts.some((h) => h.rescued || h.ignored));

	async function load() {
		const { data, error: err } = await api.GET('/api/v1/jobs/{jobId}/hosts', {
			params: {
				path: { jobId },
				query: visible
					? {
							limit: pageSize,
							offset,
							status: status ? [status] : undefined,
							search: query || undefined
						}
					: { limit: 1 }
			}
		});
		if (!data) {
			error = errorMessage(err);
			return;
		}
		error = '';
		counts = data.counts;
		if (visible) {
			hosts = data.items;
			total = data.total;
		}
	}

	// Load on opening, on filtering and paging, and (at most every 1.5s)
	// when the results change.
	let timer: ReturnType<typeof setTimeout> | undefined;
	let lastLoad = 0;
	$effect(() => {
		void [jobId, visible, status, query, offset, changed];
		const wait = Math.max(0, lastLoad + 1500 - Date.now());
		clearTimeout(timer);
		timer = setTimeout(() => {
			lastLoad = Date.now();
			load();
			if (selected && visible && !eventsMore) loadEvents(selected, true);
		}, wait);
		return () => clearTimeout(timer);
	});

	// Search as typed, after a pause.
	$effect(() => {
		const s = search.trim();
		const t = setTimeout(() => {
			if (s !== query) {
				query = s;
				offset = 0;
			}
		}, 300);
		return () => clearTimeout(t);
	});

	/** Loads the selected host's task results: the first page, or (more)
	 * those after the ones shown. */
	async function loadEvents(host: string, more: boolean) {
		const after = more ? (events.at(-1)?.seq ?? 0) : 0;
		const { data, error: err } = await api.GET('/api/v1/jobs/{jobId}/events', {
			params: { path: { jobId }, query: { hostName: host, afterSeq: after, limit: eventsPage } }
		});
		if (selected !== host) return;
		if (!data) {
			eventsError = errorMessage(err);
			return;
		}
		eventsError = '';
		const results = data.items.filter((e) => hostEventTypes.includes(e.type));
		events = more ? [...events, ...results] : results;
		eventsMore = data.items.length === eventsPage;
	}

	function select(host: string) {
		if (selected === host) {
			selected = null;
			return;
		}
		selected = host;
		events = [];
		eventsMore = false;
		eventsError = '';
		loadEvents(host, false);
	}

	function filter(s: JobHostStatus | '') {
		status = s;
		offset = 0;
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
</script>

<section class="card">
	<h2>Hosts</h2>
	{#if error}
		<p class="error" role="alert">{error}</p>
	{/if}
	{#if all === 0}
		<p class="muted">{finished ? 'No host results.' : 'No host results yet.'}</p>
	{:else}
		<div class="filters">
			<div class="statuses" role="group" aria-label="Filter by result">
				<button class="chip" aria-pressed={status === ''} onclick={() => filter('')}>
					All <span class="count">{all}</span>
				</button>
				{#each statuses as s (s.value)}
					{#if counts?.[s.value]}
						<button class="chip" aria-pressed={status === s.value} onclick={() => filter(s.value)}>
							{s.label} <span class="count">{counts[s.value]}</span>
						</button>
					{/if}
				{/each}
			</div>
			<input type="search" placeholder="Find a host" aria-label="Find a host" bind:value={search} />
		</div>

		{#if hosts.length === 0}
			<p class="muted">No hosts match.</p>
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
							{#if extras}
								<th class="num">Rescued</th>
								<th class="num">Ignored</th>
							{/if}
						</tr>
					</thead>
					<tbody>
						{#each hosts as h (h.hostName)}
							<tr class:selected={selected === h.hostName}>
								<td>
									<button
										class="link"
										aria-expanded={selected === h.hostName}
										onclick={() => select(h.hostName)}>{h.hostName}</button
									>
								</td>
								<td><StatusBadge status={h.status} /></td>
								<td class="num">{h.ok}</td>
								<td class="num">{h.changed}</td>
								<td class="num">{h.failed}</td>
								<td class="num">{h.unreachable}</td>
								<td class="num">{h.skipped}</td>
								{#if extras}
									<td class="num">{h.rescued}</td>
									<td class="num">{h.ignored}</td>
								{/if}
							</tr>
							{#if selected === h.hostName}
								<tr class="host-detail">
									<td colspan={extras ? 9 : 7}>
										{#if eventsError}
											<p class="error">{eventsError}</p>
										{:else if events.length === 0}
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
													{#each events as e (e.seq)}
														<tr>
															<td>
																{e.task?.name ?? ''}
																{#if e.task?.play}<div class="muted small">{e.task.play}</div>{/if}
															</td>
															<td>
																<StatusBadge status={outcome(e)} />
																{#if e.data?.ignored}<span class="muted small">ignored</span>{/if}
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
											{#if eventsMore}
												<button
													class="secondary small"
													onclick={() => loadEvents(h.hostName, true)}
												>
													Load more
												</button>
											{/if}
										{/if}
									</td>
								</tr>
							{/if}
						{/each}
					</tbody>
				</table>
			</div>
			{#if total > pageSize}
				<div class="pager">
					<button
						class="secondary"
						disabled={offset === 0}
						onclick={() => (offset = Math.max(0, offset - pageSize))}>Previous</button
					>
					<span class="muted">{offset + 1}–{Math.min(offset + pageSize, total)} of {total}</span>
					<button
						class="secondary"
						disabled={offset + pageSize >= total}
						onclick={() => (offset += pageSize)}>Next</button
					>
				</div>
			{/if}
		{/if}
	{/if}
</section>

<style>
	.filters {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.75rem;
		margin-bottom: 0.75rem;
	}

	.statuses {
		display: flex;
		flex-wrap: wrap;
		gap: 0.4rem;
		flex: 1;
	}

	.filters input {
		width: auto;
		min-width: 14rem;
	}

	.chip {
		padding: 0.25rem 0.7rem;
		border: 1px solid var(--border);
		border-radius: 999px;
		background: var(--surface);
		color: var(--text);
		font-size: 0.85rem;
	}

	.chip[aria-pressed='true'] {
		border-color: var(--accent);
		background: var(--accent);
		color: var(--accent-text);
	}

	.count {
		opacity: 0.75;
		font-variant-numeric: tabular-nums;
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

	button.small {
		margin-top: 0.5rem;
		padding: 0.25rem 0.6rem;
	}

	.pager {
		display: flex;
		justify-content: flex-end;
		align-items: center;
		gap: 1rem;
		margin-top: 1rem;
	}
</style>
