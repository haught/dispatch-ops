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
		type OverridableField
	} from '#lib/api/client.js';
	import LaunchDialog from '#lib/components/LaunchDialog.svelte';
	import ResourceAccess from '#lib/components/ResourceAccess.svelte';
	import Schedules from '#lib/components/Schedules.svelte';
	import StatusBadge from '#lib/components/StatusBadge.svelte';
	import TemplateForm from '#lib/components/TemplateForm.svelte';
	import { fmtDuration, fmtSeconds, fmtTime, fmtVars } from '#lib/format.js';
	import { can, loadMyGrants } from '#lib/permissions.svelte.js';
	import { loadTemplateRefs, noRefs, type Ref, type TemplateRefs } from '#lib/templateRefs.js';

	let template = $state<JobTemplate | null>(null);
	let jobs = $state<Job[]>([]);
	let targets = $state<NotificationTarget[]>([]);
	// Chosen events per target ID.
	let chosen = $state<Record<string, NotificationEvent[]>>({});
	let error = $state('');
	let launchOpen = $state(false);
	let editing = $state(false);

	// Names of what the template uses.
	let uses = $state<TemplateRefs>(noRefs);

	let notifSaving = $state(false);
	let notifMessage = $state('');
	let notifError = $state('');

	const id = $derived(page.params.id ?? '');
	const canAdmin = $derived(can('job_template', id, 'admin'));
	const prompts = (f: OverridableField) => Boolean(template?.allowOverrides.includes(f));

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
		uses = await loadTemplateRefs(t);
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

{#snippet prompt(field: OverridableField)}
	{#if prompts(field)}<span class="badge accent">prompt on launch</span>{/if}
{/snippet}

{#snippet value(v: string, blank: string)}
	{#if v}<code>{v}</code>{:else}<span class="muted">{blank}</span>{/if}
{/snippet}

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
		<button onclick={() => (launchOpen = true)}>Launch</button>
	</div>
	{#if launchOpen}
		<LaunchDialog {template} onclose={() => (launchOpen = false)} />
	{/if}
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
				{#if template.inventoryId || prompts('inventory')}
					<dt>Inventory {@render prompt('inventory')}</dt>
					<dd>
						{#if template.inventoryId}
							{@render link(`/inventories/${template.inventoryId}`, uses.inventory)}
						{:else}<span class="muted">none</span>{/if}
					</dd>
				{/if}
				{#if template.credentialIds.length}
					<dt>Credentials</dt>
					<dd>
						{#each uses.credentials as c, i (c.id)}{#if i},
							{/if}{@render link(`/credentials/${c.id}`, c)}{/each}
					</dd>
				{/if}
				<dt>Runs {@render prompt('command')}</dt>
				<dd>
					<code
						>{template.jobType === 'container'
							? template.command.join(' ')
							: [template.entrypoint, ...template.arguments].join(' ')}</code
					>
				</dd>
				{#if template.limit || prompts('limit')}
					<dt>Limit {@render prompt('limit')}</dt>
					<dd>{@render value(template.limit, 'all hosts')}</dd>
				{/if}
				{#if template.tags || prompts('tags')}
					<dt>Tags {@render prompt('tags')}</dt>
					<dd>{@render value(template.tags, 'all tasks')}</dd>
				{/if}
				{#if template.skipTags || prompts('skipTags')}
					<dt>Skip tags {@render prompt('skipTags')}</dt>
					<dd>{@render value(template.skipTags, 'none')}</dd>
				{/if}
				{#if template.verbosity}
					<dt>Verbosity</dt>
					<dd><code>-{'v'.repeat(template.verbosity)}</code></dd>
				{/if}
				{#if fmtVars(template.extraVars) || prompts('extraVars')}
					<dt>Extra variables {@render prompt('extraVars')}</dt>
					<dd>
						{#if fmtVars(template.extraVars)}
							<pre class="code">{fmtVars(template.extraVars)}</pre>
						{:else}<span class="muted">none</span>{/if}
					</dd>
				{/if}
				<dt>Timeout</dt>
				<dd>{fmtSeconds(template.timeoutSeconds)}</dd>
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
