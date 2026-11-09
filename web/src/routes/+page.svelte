<script lang="ts">
	import {
		api,
		errorMessage,
		type AuditEntry,
		type DashboardStats,
		type Health,
		type Job,
		type JobStatus,
		type Schedule,
		type StatsWindow
	} from '#lib/api/client.js';
	import ActivityChart from '#lib/components/dashboard/ActivityChart.svelte';
	import KpiTile from '#lib/components/dashboard/KpiTile.svelte';
	import NeedsAttention from '#lib/components/dashboard/NeedsAttention.svelte';
	import NotificationHealth from '#lib/components/dashboard/NotificationHealth.svelte';
	import ProblemHosts from '#lib/components/dashboard/ProblemHosts.svelte';
	import RecentChanges from '#lib/components/dashboard/RecentChanges.svelte';
	import RecentJobs from '#lib/components/dashboard/RecentJobs.svelte';
	import RunningJobs from '#lib/components/dashboard/RunningJobs.svelte';
	import SyncHealth from '#lib/components/dashboard/SyncHealth.svelte';
	import SystemCard from '#lib/components/dashboard/SystemCard.svelte';
	import TemplateLeaderboard from '#lib/components/dashboard/TemplateLeaderboard.svelte';
	import UpcomingSchedules from '#lib/components/dashboard/UpcomingSchedules.svelte';
	import { fmtPercent, fmtSeconds } from '#lib/format.js';
	import { session } from '#lib/session.svelte.js';

	const windows: { value: StatsWindow; label: string; period: string; prev: string; ms: number }[] =
		[
			{ value: '24h', label: '24h', period: 'last 24 hours', prev: 'prev 24h', ms: 24 * 3600e3 },
			{ value: '7d', label: '7 days', period: 'last 7 days', prev: 'prev 7d', ms: 7 * 24 * 3600e3 },
			{
				value: '30d',
				label: '30 days',
				period: 'last 30 days',
				prev: 'prev 30d',
				ms: 30 * 24 * 3600e3
			}
		];
	const windowKey = 'dashboard.window';
	const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;

	function storedWindow(): StatsWindow {
		try {
			const v = localStorage.getItem(windowKey);
			if (windows.some((w) => w.value === v)) return v as StatsWindow;
		} catch {
			// Storage may be unavailable; fall back to the default.
		}
		return '7d';
	}

	let windowSel = $state<StatsWindow>(storedWindow());
	const win = $derived(windows.find((w) => w.value === windowSel)!);

	let now = $state(Date.now());
	let health = $state<Health | null>(null);
	let stats = $state<DashboardStats | null>(null);
	let statsError = $state('');
	let active = $state<Job[]>([]);
	let failedJobs = $state<Job[]>([]);
	let recent = $state<Job[]>([]);
	let schedules = $state<Schedule[]>([]);
	let audit = $state<AuditEntry[]>([]);
	let hasTemplates = $state<boolean | null>(null);

	const canAudit = $derived(Boolean(session.user?.isAdmin || session.user?.isAuditor));
	const attention = $derived(
		failedJobs
			.filter((j) => new Date(j.createdAt).getTime() >= now - win.ms)
			.sort((a, b) => new Date(b.createdAt).getTime() - new Date(a.createdAt).getTime())
			.slice(0, 5)
	);

	async function jobs(query: { status?: JobStatus; limit: number }): Promise<Job[]> {
		const { data } = await api.GET('/api/v1/jobs', { params: { query } });
		return data?.items ?? [];
	}

	// What's happening now: refreshed often.
	async function loadLive() {
		const [running, waiting, pending, failed, errored, latest, h] = await Promise.all([
			jobs({ status: 'running', limit: 20 }),
			jobs({ status: 'waiting', limit: 20 }),
			jobs({ status: 'pending', limit: 20 }),
			jobs({ status: 'failed', limit: 10 }),
			jobs({ status: 'error', limit: 10 }),
			jobs({ limit: 8 }),
			api.GET('/api/v1/health')
		]);
		active = [...running, ...waiting, ...pending];
		failedJobs = [...failed, ...errored];
		recent = latest;
		health = h.data ?? (h.error as Health | undefined) ?? null;
	}

	// Things that change slowly.
	async function loadSlow() {
		const [sch, tmpl, log] = await Promise.all([
			api.GET('/api/v1/schedules', { params: { query: { limit: 500 } } }),
			api.GET('/api/v1/job-templates', { params: { query: { limit: 1 } } }),
			canAudit ? api.GET('/api/v1/audit-log', { params: { query: { limit: 8 } } }) : null
		]);
		schedules = sch.data?.items ?? [];
		hasTemplates = tmpl.data ? tmpl.data.total > 0 : null;
		audit = log?.data?.items ?? [];
	}

	async function loadStats(w: StatsWindow) {
		const { data, error } = await api.GET('/api/v1/stats', {
			params: { query: { window: w, tz } }
		});
		if (w !== windowSel) return; // the window changed while loading
		if (data) {
			stats = data;
			statsError = '';
		} else {
			statsError = errorMessage(error);
		}
	}

	function every(ms: number, fn: () => void): () => void {
		const timer = setInterval(() => {
			if (document.visibilityState === 'visible') fn();
		}, ms);
		return () => clearInterval(timer);
	}

	$effect(() => {
		loadLive();
		loadSlow();
		const stops = [
			every(15_000, loadLive),
			every(60_000, loadSlow),
			every(5_000, () => (now = Date.now()))
		];
		const onVisible = () => {
			if (document.visibilityState === 'visible') {
				now = Date.now();
				loadLive();
			}
		};
		document.addEventListener('visibilitychange', onVisible);
		return () => {
			stops.forEach((stop) => stop());
			document.removeEventListener('visibilitychange', onVisible);
		};
	});

	$effect(() => {
		const w = windowSel;
		try {
			localStorage.setItem(windowKey, w);
		} catch {
			// Not remembered; that's fine.
		}
		loadStats(w);
		return every(60_000, () => loadStats(w));
	});

	const t = $derived(stats?.totals);
	const p = $derived(stats?.previousTotals);
	const secs = (v: number | null | undefined) => (v == null ? '—' : fmtSeconds(v));
</script>

<div class="header">
	<h1>Welcome, {session.user?.displayName || session.user?.username}</h1>
	<div class="seg" role="group" aria-label="Statistics window">
		{#each windows as w (w.value)}
			<button
				class="secondary"
				class:active={windowSel === w.value}
				aria-pressed={windowSel === w.value}
				onclick={() => (windowSel = w.value)}>{w.label}</button
			>
		{/each}
	</div>
</div>

{#if hasTemplates === false}
	<section class="card getting-started">
		<h2>Getting started</h2>
		<p>
			Jobs run a playbook, script or command from a <a href="/projects">project</a>, in an
			<a href="/execution-environments">execution environment</a>, against an
			<a href="/inventories">inventory</a>, with secrets from
			<a href="/credentials">credentials</a>.
		</p>
		<p class="muted">
			Set those up, then create a <a href="/templates">job template</a> and launch it. You can
			follow its output live, run it on a <a href="/schedules">schedule</a>, and get
			<a href="/notifications">notified</a> when it finishes.
		</p>
	</section>
{/if}

<div class="kpis">
	<KpiTile
		label="Jobs run"
		value={t ? String(t.total) : '—'}
		current={t?.total}
		previous={p?.total}
		better="neutral"
		period={win.prev}
	/>
	<KpiTile
		label="Success rate"
		value={fmtPercent(t?.successRate)}
		current={t?.successRate}
		previous={p?.successRate}
		points
		period={win.prev}
	/>
	<KpiTile
		label="Median run time"
		value={secs(t?.p50DurationSeconds)}
		current={t?.p50DurationSeconds}
		previous={p?.p50DurationSeconds}
		better="down"
		period={win.prev}
	/>
	<KpiTile
		label="p95 run time"
		value={secs(t?.p95DurationSeconds)}
		current={t?.p95DurationSeconds}
		previous={p?.p95DurationSeconds}
		better="down"
		period={win.prev}
	/>
	<KpiTile
		label="Median queue wait"
		value={secs(t?.p50QueueWaitSeconds)}
		current={t?.p50QueueWaitSeconds}
		previous={p?.p50QueueWaitSeconds}
		better="down"
		period={win.prev}
	/>
</div>

<section class="card chart-card">
	<h2>Activity <span class="muted sub">· jobs per {stats?.bucket ?? 'day'}, {win.period}</span></h2>
	{#if statsError}
		<p class="error" role="alert">{statsError}</p>
	{:else if stats}
		<ActivityChart series={stats.series} bucket={stats.bucket} />
	{:else}
		<p class="muted">Loading…</p>
	{/if}
</section>

<h2 class="group">Now</h2>
<div class="grid">
	<RunningJobs jobs={active} {now} />
	<NeedsAttention jobs={attention} {now} period={win.period} />
	<RecentJobs jobs={recent} {now} />
	<UpcomingSchedules {schedules} {now} />
</div>

{#if stats}
	<h2 class="group">Templates &amp; hosts <span class="muted sub">· {win.period}</span></h2>
	<div class="grid wide-first">
		<TemplateLeaderboard templates={stats.templates} period={win.period} />
		<ProblemHosts hosts={stats.problemHosts} {now} period={win.period} />
	</div>
{/if}

<h2 class="group">Health</h2>
<div class="grid">
	<SystemCard {health} queue={stats?.queue} />
	{#if stats}
		<SyncHealth syncs={stats.inventorySyncs} {now} />
		{#if stats.notifications}
			<NotificationHealth stats={stats.notifications} {now} period={win.period} />
		{/if}
	{/if}
	{#if canAudit}
		<RecentChanges entries={audit} {now} />
	{/if}
</div>

<style>
	.header {
		display: flex;
		justify-content: space-between;
		align-items: center;
		flex-wrap: wrap;
		gap: 0.5rem 1rem;
		margin-bottom: 1rem;
	}

	.header h1 {
		margin: 0;
	}

	.seg {
		display: inline-flex;
	}

	.seg button {
		padding: 0.3rem 0.75rem;
		font-size: 0.85rem;
		border-radius: 0;
	}

	.seg button:first-child {
		border-radius: 6px 0 0 6px;
	}

	.seg button:last-child {
		border-radius: 0 6px 6px 0;
	}

	.seg button + button {
		border-left: none;
	}

	.seg button.active {
		background: var(--accent);
		border-color: var(--accent);
		color: var(--accent-text);
	}

	.getting-started {
		margin-bottom: 1rem;
	}

	.getting-started p:last-child {
		margin-bottom: 0;
	}

	.kpis {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(170px, 1fr));
		gap: 0.75rem;
		margin-bottom: 1rem;
	}

	.chart-card {
		margin-bottom: 0.5rem;
	}

	.sub {
		font-weight: normal;
		font-size: 0.9rem;
	}

	h2.group {
		margin: 1.5rem 0 0.75rem;
		font-size: 0.85rem;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--muted);
	}

	h2.group .sub {
		text-transform: none;
		letter-spacing: normal;
	}

	.grid {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(320px, 1fr));
		gap: 1rem;
		align-items: start;
	}

	.grid > :global(section.card) {
		margin: 0;
		min-width: 0;
	}

	@media (min-width: 900px) {
		.grid.wide-first {
			grid-template-columns: 3fr 2fr;
		}
	}
</style>
