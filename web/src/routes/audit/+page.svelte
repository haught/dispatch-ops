<script lang="ts">
	import {
		api,
		errorMessage,
		type AuditAction,
		type AuditEntry,
		type AuditResourceType
	} from '#lib/api/client.js';
	import { fmtTime } from '#lib/format.js';

	const pageSize = 50;
	const actions: AuditAction[] = [
		'create',
		'update',
		'delete',
		'launch',
		'cancel',
		'login',
		'login_throttled',
		'grant',
		'revoke',
		'add_member',
		'remove_member'
	];
	const types: AuditResourceType[] = [
		'inventory',
		'host',
		'group',
		'credential',
		'execution_environment',
		'project',
		'job_template',
		'job',
		'user',
		'team',
		'api_token'
	];

	let entries = $state<AuditEntry[]>([]);
	let total = $state(0);
	let offset = $state(0);
	let action = $state<AuditAction | ''>('');
	let resourceType = $state<AuditResourceType | ''>('');
	let open = $state<number | null>(null);
	let loading = $state(true);
	let error = $state('');

	async function load() {
		const { data, error: err } = await api.GET('/api/v1/audit-log', {
			params: {
				query: {
					limit: pageSize,
					offset,
					action: action || undefined,
					resourceType: resourceType || undefined
				}
			}
		});
		loading = false;
		if (data) {
			entries = data.items;
			total = data.total;
			error = '';
		} else {
			error = errorMessage(err);
		}
	}

	$effect(() => {
		void action;
		void resourceType;
		void offset;
		load();
	});

	function link(e: AuditEntry): string | null {
		if (!e.resourceId) return null;
		switch (e.resourceType) {
			case 'job':
				return `/jobs/${e.resourceId}`;
			case 'user':
				return `/users/${e.resourceId}`;
			case 'team':
				return `/teams/${e.resourceId}`;
		}
		return null;
	}

	function json(v: unknown): string {
		return JSON.stringify(v, null, 2);
	}
</script>

<h1>Audit log</h1>
<p class="muted">
	Every change, launch and sign-in, newest first. Updates show only the fields that changed.
</p>

<div class="toolbar">
	<label>
		Action
		<select bind:value={action} onchange={() => (offset = 0)}>
			<option value="">All</option>
			{#each actions as a (a)}<option value={a}>{a}</option>{/each}
		</select>
	</label>
	<label>
		Resource
		<select bind:value={resourceType} onchange={() => (offset = 0)}>
			<option value="">All</option>
			{#each types as t (t)}<option value={t}>{t}</option>{/each}
		</select>
	</label>
</div>

{#if error}
	<p class="error" role="alert">{error}</p>
{/if}

<section class="card">
	{#if loading}
		<p class="muted">Loading…</p>
	{:else if entries.length === 0}
		<p class="muted">No entries.</p>
	{:else}
		<div class="table-wrap">
			<table>
				<thead>
					<tr>
						<th>Time</th>
						<th>Who</th>
						<th>Action</th>
						<th>Resource</th>
						<th></th>
					</tr>
				</thead>
				<tbody>
					{#each entries as e (e.id)}
						{@const href = link(e)}
						<tr>
							<td class="nowrap">{fmtTime(e.time)}</td>
							<td>
								{#if e.actor}<a href="/users/{e.actor.id}">{e.actorName}</a>{:else}{e.actorName ||
										'—'}{/if}
								<div class="muted small">{e.authMethod}</div>
							</td>
							<td><span class="badge">{e.action}</span></td>
							<td>
								<span class="muted small">{e.resourceType}</span>
								{#if href}<a {href}>{e.resourceName || e.resourceId}</a>{:else}{e.resourceName}{/if}
							</td>
							<td class="row-actions">
								{#if e.before || e.after}
									<button class="secondary" onclick={() => (open = open === e.id ? null : e.id)}>
										{open === e.id ? 'Hide' : 'Details'}
									</button>
								{/if}
							</td>
						</tr>
						{#if open === e.id}
							<tr class="details">
								<td colspan="5">
									<div class="diff">
										{#if e.before}
											<div>
												<div class="muted small">Before</div>
												<pre>{json(e.before)}</pre>
											</div>
										{/if}
										{#if e.after}
											<div>
												<div class="muted small">After</div>
												<pre>{json(e.after)}</pre>
											</div>
										{/if}
									</div>
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
		gap: 1rem;
		margin-bottom: 1rem;
	}

	.toolbar label {
		margin: 0;
		min-width: 180px;
	}

	.nowrap {
		white-space: nowrap;
	}

	.diff {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
		gap: 1rem;
	}

	pre {
		margin: 0.25rem 0 0;
		padding: 0.75rem;
		background: var(--bg);
		border-radius: 6px;
		overflow-x: auto;
		font-size: 0.85rem;
	}

	tr.details td {
		background: var(--surface);
	}

	.pager {
		display: flex;
		justify-content: flex-end;
		align-items: center;
		gap: 1rem;
		margin-top: 1rem;
	}
</style>
