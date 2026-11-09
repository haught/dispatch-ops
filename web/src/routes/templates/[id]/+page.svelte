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
		type NotificationTarget
	} from '#lib/api/client.js';
	import ResourceAccess from '#lib/components/ResourceAccess.svelte';
	import Schedules from '#lib/components/Schedules.svelte';
	import StatusBadge from '#lib/components/StatusBadge.svelte';
	import TemplateForm from '#lib/components/TemplateForm.svelte';
	import { fmtDuration, fmtSeconds, fmtTime, fmtVars } from '#lib/format.js';
	import { can, loadMyGrants } from '#lib/permissions.svelte.js';

	let template = $state<JobTemplate | null>(null);
	let jobs = $state<Job[]>([]);
	let targets = $state<NotificationTarget[]>([]);
	// Chosen events per target ID.
	let chosen = $state<Record<string, NotificationEvent[]>>({});
	let error = $state('');
	let launching = $state(false);
	let editing = $state(false);

	// Names of what the template uses; null where the caller can't see it.
	type Ref = { id: string; name: string | null };
	let uses = $state<{
		ee: Ref | null;
		project: Ref | null;
		inventory: Ref | null;
		credentials: Ref[];
	}>({ ee: null, project: null, inventory: null, credentials: [] });

	let notifSaving = $state(false);
	let notifMessage = $state('');
	let notifError = $state('');

	const id = $derived(page.params.id ?? '');
	const canAdmin = $derived(can('job_template', id, 'admin'));

	$effect(() => {
		loadMyGrants();
	});

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

	async function loadUses(t: JobTemplate) {
		const ref = (id: string, data: { name: string } | undefined): Ref => ({
			id,
			name: data?.name ?? null
		});
		const [ee, project, inventory, ...credentials] = await Promise.all([
			api
				.GET('/api/v1/execution-environments/{executionEnvironmentId}', {
					params: { path: { executionEnvironmentId: t.executionEnvironmentId } }
				})
				.then((r) => ref(t.executionEnvironmentId, r.data)),
			t.projectId
				? api
						.GET('/api/v1/projects/{projectId}', { params: { path: { projectId: t.projectId } } })
						.then((r) => ref(t.projectId!, r.data))
				: null,
			t.inventoryId
				? api
						.GET('/api/v1/inventories/{inventoryId}', {
							params: { path: { inventoryId: t.inventoryId } }
						})
						.then((r) => ref(t.inventoryId!, r.data))
				: null,
			...t.credentialIds.map((cid) =>
				api
					.GET('/api/v1/credentials/{credentialId}', { params: { path: { credentialId: cid } } })
					.then((r) => ref(cid, r.data))
			)
		]);
		uses = { ee, project, inventory, credentials: credentials.filter((c) => c !== null) };
	}

	$effect(() => {
		const tid = id;
		if (!tid) return;
		template = null;
		editing = false;
		error = '';
		api
			.GET('/api/v1/job-templates/{jobTemplateId}', { params: { path: { jobTemplateId: tid } } })
			.then(({ data, error: err }) => {
				if (!data) {
					error = errorMessage(err);
					return;
				}
				template = data;
				loadUses(data);
				loadJobs(tid);
				loadNotifications(tid);
			});
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

	function saved(t: JobTemplate) {
		template = t;
		editing = false;
		loadUses(t);
	}

	async function remove() {
		if (!template) return;
		const ok = confirm(
			`Delete the job template ${template.name}? Its schedules and notifications are deleted too; its jobs are kept.`
		);
		if (!ok) return;
		error = '';
		const { error: err } = await api.DELETE('/api/v1/job-templates/{jobTemplateId}', {
			params: { path: { jobTemplateId: template.id } }
		});
		if (err) error = errorMessage(err);
		else goto('/templates');
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

{#snippet link(href: string, ref: Ref | null)}
	{#if ref?.name}<a {href}>{ref.name}</a>{:else if ref}<span class="muted">not visible to you</span
		>{:else}…{/if}
{/snippet}

{#if template}
	<div class="page-header">
		<h1>{template.name}</h1>
		<span class="badge">{template.jobType}</span>
		<span class="spacer"></span>
		{#if canAdmin && !editing}
			<button class="secondary" onclick={() => (editing = true)}>Edit</button>
			<button class="danger" onclick={remove}>Delete</button>
		{/if}
		<button onclick={launch} disabled={launching}>{launching ? 'Launching…' : 'Launch'}</button>
	</div>
	{#if template.description}<p class="muted">{template.description}</p>{/if}
	{#if error}<p class="error" role="alert">{error}</p>{/if}

	{#if editing}
		<section class="card">
			<h2>Edit job template</h2>
			{#key template.id}
				<TemplateForm {template} onsaved={saved} oncancel={() => (editing = false)} />
			{/key}
		</section>
	{:else}
		<section class="card">
			<dl>
				<dt>Execution environment</dt>
				<dd>{@render link('/execution-environments', uses.ee)}</dd>
				{#if template.projectId}
					<dt>Project</dt>
					<dd>{@render link(`/projects/${template.projectId}`, uses.project)}</dd>
				{/if}
				{#if template.inventoryId}
					<dt>Inventory</dt>
					<dd>{@render link(`/inventories/${template.inventoryId}`, uses.inventory)}</dd>
				{/if}
				{#if template.credentialIds.length}
					<dt>Credentials</dt>
					<dd>
						{#each uses.credentials as c, i (c.id)}{#if i},
							{/if}{@render link(`/credentials/${c.id}`, c)}{/each}
					</dd>
				{/if}
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
				{#if template.tags}
					<dt>Tags</dt>
					<dd><code>{template.tags}</code></dd>
				{/if}
				{#if template.skipTags}
					<dt>Skip tags</dt>
					<dd><code>{template.skipTags}</code></dd>
				{/if}
				{#if template.verbosity}
					<dt>Verbosity</dt>
					<dd><code>-{'v'.repeat(template.verbosity)}</code></dd>
				{/if}
				{#if fmtVars(template.extraVars)}
					<dt>Extra variables</dt>
					<dd><pre class="code">{fmtVars(template.extraVars)}</pre></dd>
				{/if}
				<dt>Timeout</dt>
				<dd>{fmtSeconds(template.timeoutSeconds)}</dd>
				<dt>Overridable at launch</dt>
				<dd>{template.allowOverrides.length ? template.allowOverrides.join(', ') : 'nothing'}</dd>
			</dl>
		</section>
	{/if}

	<section class="card">
		<h2>Schedules</h2>
		<Schedules jobTemplateId={template.id} allowLimit={template.allowOverrides.includes('limit')} />
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

	<ResourceAccess type="job_template" id={template.id} />
{:else if error}
	<p class="error" role="alert">{error}</p>
	<p><a href="/templates">Back to job templates</a></p>
{:else}
	<p class="muted">Loading…</p>
{/if}

<style>
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
</style>
