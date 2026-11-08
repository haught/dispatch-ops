<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import {
		api,
		errorMessage,
		notificationEvents,
		type Job,
		type JobTemplate,
		type NotificationEvent,
		type NotificationTarget,
		type Schedule
	} from '#lib/api/client.js';
	import StatusBadge from '#lib/components/StatusBadge.svelte';
	import { fmtDuration, fmtSeconds, fmtTime } from '#lib/format.js';

	let template = $state<JobTemplate | null>(null);
	let schedules = $state<Schedule[]>([]);
	let jobs = $state<Job[]>([]);
	let targets = $state<NotificationTarget[]>([]);
	// Chosen events per target ID.
	let chosen = $state<Record<string, NotificationEvent[]>>({});
	let error = $state('');
	let launching = $state(false);

	// New schedule form.
	let scName = $state('');
	let scCron = $state('0 3 * * *');
	let scTimezone = $state(Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC');
	let scLimit = $state('');
	let scError = $state('');
	let preview = $state<string[]>([]);
	let previewError = $state('');

	let notifSaving = $state(false);
	let notifMessage = $state('');
	let notifError = $state('');

	const id = $derived(page.params.id ?? '');

	async function loadSchedules(tid: string) {
		const { data } = await api.GET('/api/v1/schedules', {
			params: { query: { jobTemplateId: tid, limit: 500 } }
		});
		if (data) schedules = data.items;
	}

	async function loadJobs(tid: string) {
		const { data } = await api.GET('/api/v1/jobs', {
			params: { query: { jobTemplateId: tid, limit: 10 } }
		});
		if (data) jobs = data.items;
	}

	async function loadNotifications(tid: string) {
		const [all, mine] = await Promise.all([
			api.GET('/api/v1/notification-targets', { params: { query: { limit: 500 } } }),
			api.GET('/api/v1/job-templates/{jobTemplateId}/notifications', {
				params: { path: { jobTemplateId: tid } }
			})
		]);
		targets = all.data?.items ?? [];
		const c: Record<string, NotificationEvent[]> = {};
		for (const t of targets) c[t.id] = [];
		for (const n of mine.data?.items ?? []) c[n.targetId] = [...n.events];
		chosen = c;
	}

	$effect(() => {
		const tid = id;
		if (!tid) return;
		template = null;
		error = '';
		api
			.GET('/api/v1/job-templates/{jobTemplateId}', { params: { path: { jobTemplateId: tid } } })
			.then(({ data, error: err }) => {
				if (!data) {
					error = errorMessage(err);
					return;
				}
				template = data;
				loadSchedules(tid);
				loadJobs(tid);
				loadNotifications(tid);
			});
	});

	// Check the cron expression as it is typed.
	$effect(() => {
		const cron = scCron.trim();
		const timezone = scTimezone.trim();
		if (!cron) {
			preview = [];
			previewError = '';
			return;
		}
		const timer = setTimeout(async () => {
			const { data, error: err } = await api.GET('/api/v1/schedule-preview', {
				params: { query: { cron, timezone: timezone || undefined, count: 3 } }
			});
			if (data) {
				preview = data.runs;
				previewError = '';
			} else {
				preview = [];
				previewError = errorMessage(err);
			}
		}, 300);
		return () => clearTimeout(timer);
	});

	async function launch() {
		if (!template) return;
		launching = true;
		error = '';
		const { data, error: err } = await api.POST('/api/v1/job-templates/{jobTemplateId}/launch', {
			params: { path: { jobTemplateId: template.id } },
			body: {}
		});
		launching = false;
		if (data) goto(`/jobs/${data.id}`);
		else error = errorMessage(err);
	}

	async function createSchedule(event: SubmitEvent) {
		event.preventDefault();
		if (!template) return;
		scError = '';
		const { error: err } = await api.POST('/api/v1/schedules', {
			body: {
				name: scName,
				jobTemplateId: template.id,
				cron: scCron.trim(),
				timezone: scTimezone.trim() || 'UTC',
				enabled: true,
				overrides: scLimit ? { limit: scLimit } : undefined
			}
		});
		if (err) {
			scError = errorMessage(err);
			return;
		}
		scName = scLimit = '';
		await loadSchedules(template.id);
	}

	async function setEnabled(s: Schedule, enabled: boolean) {
		const { error: err } = await api.PATCH('/api/v1/schedules/{scheduleId}', {
			params: { path: { scheduleId: s.id } },
			body: { enabled }
		});
		if (err) scError = errorMessage(err);
		await loadSchedules(s.jobTemplateId);
	}

	async function removeSchedule(s: Schedule) {
		const { error: err } = await api.DELETE('/api/v1/schedules/{scheduleId}', {
			params: { path: { scheduleId: s.id } }
		});
		if (err) scError = errorMessage(err);
		await loadSchedules(s.jobTemplateId);
	}

	function toggleEvent(targetId: string, ev: NotificationEvent, on: boolean) {
		const cur = chosen[targetId] ?? [];
		chosen[targetId] = on ? [...cur, ev] : cur.filter((e) => e !== ev);
		notifMessage = '';
	}

	async function saveNotifications() {
		if (!template) return;
		notifSaving = true;
		notifError = notifMessage = '';
		const items = Object.entries(chosen)
			.filter(([, events]) => events.length > 0)
			.map(([targetId, events]) => ({ targetId, events }));
		const { error: err } = await api.PUT('/api/v1/job-templates/{jobTemplateId}/notifications', {
			params: { path: { jobTemplateId: template.id } },
			body: { items }
		});
		notifSaving = false;
		if (err) notifError = errorMessage(err);
		else notifMessage = 'Saved.';
	}
</script>

{#if template}
	<div class="header">
		<h1>{template.name}</h1>
		<span class="badge">{template.jobType}</span>
		<span class="spacer"></span>
		<button onclick={launch} disabled={launching}>{launching ? 'Launching…' : 'Launch'}</button>
	</div>
	{#if template.description}<p class="muted">{template.description}</p>{/if}
	{#if error}<p class="error" role="alert">{error}</p>{/if}

	<section class="card">
		<dl>
			<dt>Runs</dt>
			<dd>
				<code
					>{template.jobType === 'container'
						? template.command.join(' ')
						: [template.entrypoint, ...template.arguments].join(' ')}</code
				>
			</dd>
			{#if template.limit}
				<dt>Limit</dt>
				<dd><code>{template.limit}</code></dd>
			{/if}
			<dt>Timeout</dt>
			<dd>{fmtSeconds(template.timeoutSeconds)}</dd>
			<dt>Overridable at launch</dt>
			<dd>{template.allowOverrides.length ? template.allowOverrides.join(', ') : 'nothing'}</dd>
		</dl>
	</section>

	<section class="card">
		<h2>Schedules</h2>
		{#if scError}<p class="error" role="alert">{scError}</p>{/if}
		{#if schedules.length === 0}
			<p class="muted">Not scheduled.</p>
		{:else}
			<div class="table-wrap">
				<table>
					<thead>
						<tr>
							<th>Name</th>
							<th>When</th>
							<th>Next run</th>
							<th>Last run</th>
							<th></th>
						</tr>
					</thead>
					<tbody>
						{#each schedules as s (s.id)}
							<tr>
								<td>
									{s.name}
									{#if s.overrides.limit}<div class="muted small">
											limit <code>{s.overrides.limit}</code>
										</div>{/if}
								</td>
								<td><code>{s.cron}</code> <span class="muted small">{s.timezone}</span></td>
								<td>{s.enabled ? fmtTime(s.nextRunAt) : 'disabled'}</td>
								<td>
									{#if s.lastError}
										<span class="error small" title={s.lastError}>Failed to launch</span>
									{:else if s.lastJobId}
										<a href="/jobs/{s.lastJobId}">{fmtTime(s.lastRunAt)}</a>
									{:else}
										{fmtTime(s.lastRunAt)}
									{/if}
								</td>
								<td>
									<div class="row-actions">
										<button class="secondary" onclick={() => setEnabled(s, !s.enabled)}>
											{s.enabled ? 'Disable' : 'Enable'}
										</button>
										<button class="danger" onclick={() => removeSchedule(s)}>Delete</button>
									</div>
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}

		<h3>New schedule</h3>
		<form onsubmit={createSchedule} class="form-grid">
			<label>Name <input bind:value={scName} maxlength="255" required /></label>
			<label>
				Cron
				<input bind:value={scCron} required placeholder="0 3 * * *" />
			</label>
			<label>Time zone <input bind:value={scTimezone} placeholder="UTC" /></label>
			{#if template.allowOverrides.includes('limit')}
				<label>Limit <input bind:value={scLimit} placeholder="Template's limit" /></label>
			{/if}
			<label><button type="submit" disabled={Boolean(previewError)}>Add schedule</button></label>
		</form>
		<p class="small muted">
			Five fields (minute hour day-of-month month day-of-week), or <code>@daily</code>,
			<code>@hourly</code>, <code>@every 6h</code>…
			{#if previewError}
				<span class="error">{previewError}</span>
			{:else if preview.length}
				Next runs: {preview.map((r) => fmtTime(r)).join(' · ')}
			{/if}
		</p>
	</section>

	<section class="card">
		<h2>Notifications</h2>
		{#if targets.length === 0}
			<p class="muted">
				No notification targets yet. System administrators set them up under Notifications.
			</p>
		{:else}
			<div class="table-wrap">
				<table>
					<thead>
						<tr>
							<th>Target</th>
							{#each notificationEvents as e (e.event)}<th>{e.label}</th>{/each}
						</tr>
					</thead>
					<tbody>
						{#each targets as t (t.id)}
							<tr>
								<td>{t.name} <span class="muted small">{t.kind}</span></td>
								{#each notificationEvents as e (e.event)}
									<td>
										<input
											type="checkbox"
											aria-label="{t.name}: {e.label}"
											checked={chosen[t.id]?.includes(e.event)}
											onchange={(ev) => toggleEvent(t.id, e.event, ev.currentTarget.checked)}
										/>
									</td>
								{/each}
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
			<p>
				<button onclick={saveNotifications} disabled={notifSaving}>
					{notifSaving ? 'Saving…' : 'Save notifications'}
				</button>
				{#if notifMessage}<span class="muted">{notifMessage}</span>{/if}
				{#if notifError}<span class="error">{notifError}</span>{/if}
			</p>
		{/if}
	</section>

	<section class="card">
		<h2>Recent jobs</h2>
		{#if jobs.length === 0}
			<p class="muted">No jobs yet.</p>
		{:else}
			<div class="table-wrap">
				<table>
					<thead>
						<tr>
							<th>Status</th>
							<th>Launched</th>
							<th>By</th>
							<th>Duration</th>
						</tr>
					</thead>
					<tbody>
						{#each jobs as j (j.id)}
							<tr>
								<td><a href="/jobs/{j.id}"><StatusBadge status={j.status} /></a></td>
								<td><a href="/jobs/{j.id}">{fmtTime(j.createdAt)}</a></td>
								<td>
									{j.schedule ? `schedule ${j.schedule.name}` : (j.launchedBy?.username ?? '—')}
									<span class="muted small">{j.launchType}</span>
								</td>
								<td>{fmtDuration(j.startedAt, j.finishedAt)}</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
			<p><a href="/jobs?jobTemplateId={template.id}">All jobs</a></p>
		{/if}
	</section>
{:else if error}
	<p class="error" role="alert">{error}</p>
	<p><a href="/templates">Back to job templates</a></p>
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

	h3 {
		font-size: 1rem;
		margin: 1.25rem 0 0.5rem;
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

	input[type='checkbox'] {
		width: auto;
	}
</style>
