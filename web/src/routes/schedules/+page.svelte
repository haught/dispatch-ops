<script lang="ts">
	import { api, errorMessage, type Schedule } from '#lib/api/client.js';
	import { fmtTime } from '#lib/format.js';

	let schedules = $state<Schedule[]>([]);
	let total = $state(0);
	let search = $state('');
	let loading = $state(true);
	let error = $state('');

	async function load() {
		const { data, error: err } = await api.GET('/api/v1/schedules', {
			params: { query: { limit: 500, search: search || undefined } }
		});
		loading = false;
		if (data) {
			schedules = data.items;
			total = data.total;
			error = '';
		} else {
			error = errorMessage(err);
		}
	}

	$effect(() => {
		void search;
		load();
	});
</script>

<h1>Schedules</h1>
<p class="muted">
	Schedules launch job templates on a cron schedule. Add one from its job template's page.
</p>

<label class="search">
	Search
	<input bind:value={search} placeholder="Name contains…" />
</label>

{#if error}
	<p class="error" role="alert">{error}</p>
{/if}

<section class="card">
	{#if loading}
		<p class="muted">Loading…</p>
	{:else if schedules.length === 0}
		<p class="muted">No schedules{search ? ' match' : ' yet'}.</p>
	{:else}
		<div class="table-wrap">
			<table>
				<thead>
					<tr>
						<th>Launches</th>
						<th>Schedule</th>
						<th>When</th>
						<th>Next run</th>
						<th>Last run</th>
					</tr>
				</thead>
				<tbody>
					{#each schedules as s (s.id)}
						<tr>
							<td>
								{#if s.inventoryId}
									Sync of <a href="/inventories/{s.inventoryId}">{s.inventoryName}</a>
								{:else}
									<a href="/templates/{s.jobTemplateId}">{s.jobTemplateName}</a>
								{/if}
							</td>
							<td>
								{s.name}
								{#if s.description}<div class="muted small">{s.description}</div>{/if}
							</td>
							<td><code>{s.cron}</code> <span class="muted small">{s.timezone}</span></td>
							<td>{s.enabled ? fmtTime(s.nextRunAt) : 'disabled'}</td>
							<td>
								{#if s.lastError}
									<span class="error small" title={s.lastError}>Failed to launch</span>
								{:else if s.lastRunAt}
									<a href="/jobs?scheduleId={s.id}">{fmtTime(s.lastRunAt)}</a>
								{:else}
									—
								{/if}
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
		{#if total > schedules.length}
			<p class="muted">Showing {schedules.length} of {total}. Narrow the search to see others.</p>
		{/if}
	{/if}
</section>

<style>
	.search {
		max-width: 320px;
	}
</style>
