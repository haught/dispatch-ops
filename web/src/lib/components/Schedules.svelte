<script lang="ts">
	import { api, errorMessage, type Schedule } from '#lib/api/client.js';
	import { fmtTime } from '#lib/format.js';

	/**
	 * Lists the schedules of a job template or an inventory (its syncs) and
	 * adds new ones. Give exactly one of jobTemplateId and inventoryId.
	 */
	let {
		jobTemplateId = undefined,
		inventoryId = undefined,
		allowLimit = false,
		empty = 'Not scheduled.'
	}: {
		jobTemplateId?: string;
		inventoryId?: string;
		/** Offer a limit override (templates that allow it). */
		allowLimit?: boolean;
		empty?: string;
	} = $props();

	let schedules = $state<Schedule[]>([]);
	let error = $state('');

	let name = $state('');
	let cron = $state('0 3 * * *');
	let timezone = $state(Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC');
	let limit = $state('');
	let preview = $state<string[]>([]);
	let previewError = $state('');

	async function load() {
		const { data } = await api.GET('/api/v1/schedules', {
			params: { query: { jobTemplateId, inventoryId, limit: 500 } }
		});
		if (data) schedules = data.items;
	}

	$effect(() => {
		if (jobTemplateId || inventoryId) load();
	});

	// Check the cron expression as it is typed.
	$effect(() => {
		const expr = cron.trim();
		const tz = timezone.trim();
		if (!expr) {
			preview = [];
			previewError = '';
			return;
		}
		const timer = setTimeout(async () => {
			const { data, error: err } = await api.GET('/api/v1/schedule-preview', {
				params: { query: { cron: expr, timezone: tz || undefined, count: 3 } }
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

	async function create(event: SubmitEvent) {
		event.preventDefault();
		error = '';
		const { error: err } = await api.POST('/api/v1/schedules', {
			body: {
				name,
				jobTemplateId,
				inventoryId,
				cron: cron.trim(),
				timezone: timezone.trim() || 'UTC',
				enabled: true,
				overrides: allowLimit && limit ? { limit } : undefined
			}
		});
		if (err) {
			error = errorMessage(err);
			return;
		}
		name = limit = '';
		await load();
	}

	async function setEnabled(s: Schedule, enabled: boolean) {
		const { error: err } = await api.PATCH('/api/v1/schedules/{scheduleId}', {
			params: { path: { scheduleId: s.id } },
			body: { enabled }
		});
		if (err) error = errorMessage(err);
		await load();
	}

	async function remove(s: Schedule) {
		const { error: err } = await api.DELETE('/api/v1/schedules/{scheduleId}', {
			params: { path: { scheduleId: s.id } }
		});
		if (err) error = errorMessage(err);
		await load();
	}
</script>

{#if error}<p class="error" role="alert">{error}</p>{/if}
{#if schedules.length === 0}
	<p class="muted">{empty}</p>
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
								<button class="danger" onclick={() => remove(s)}>Delete</button>
							</div>
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
{/if}

<h3>New schedule</h3>
<form onsubmit={create} class="form-grid">
	<label>Name <input bind:value={name} maxlength="255" required /></label>
	<label>
		Cron
		<input bind:value={cron} required placeholder="0 3 * * *" />
	</label>
	<label>Time zone <input bind:value={timezone} placeholder="UTC" /></label>
	{#if allowLimit}
		<label>Limit <input bind:value={limit} placeholder="Template's limit" /></label>
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

<style>
	h3 {
		font-size: 1rem;
		margin: 1.25rem 0 0.5rem;
	}
</style>
