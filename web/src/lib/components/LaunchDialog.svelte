<script lang="ts">
	import { goto } from '$app/navigation';
	import { untrack } from 'svelte';
	import {
		api,
		errorMessage,
		type Inventory,
		type JobTemplate,
		type OverridableField
	} from '#lib/api/client.js';
	import { argvError, fmtArgv, fmtSeconds, fmtVars, parseArgv, varsError } from '#lib/format.js';
	import { buildLaunchRequest, type LaunchValues } from '#lib/overrides.js';
	import { can } from '#lib/permissions.svelte.js';
	import { loadTemplateRefs, noRefs, type Ref, type TemplateRefs } from '#lib/templateRefs.js';
	import VarsField from './VarsField.svelte';

	/**
	 * Launches a job template after showing what the job will run with, and
	 * lets the launcher change the fields the template prompts for. Only
	 * values that differ from the template's are sent.
	 */
	let { template, onclose }: { template: JobTemplate; onclose: () => void } = $props();

	const t = untrack(() => template);
	const prompted = (f: OverridableField) => t.allowOverrides.includes(f);

	let values = $state<LaunchValues>({
		inventoryId: t.inventoryId ?? '',
		extraVars: fmtVars(t.extraVars),
		limit: t.limit,
		command: fmtArgv(t.command),
		tags: t.tags,
		skipTags: t.skipTags
	});

	let dialog: HTMLDialogElement;
	let refs = $state<TemplateRefs>(noRefs);
	let inventories = $state<Inventory[]>([]);
	let launching = $state(false);
	let error = $state('');

	$effect(() => {
		dialog.showModal();
		loadTemplateRefs(t).then((r) => (refs = r));
		if (prompted('inventory')) {
			api
				.GET('/api/v1/inventories', { params: { query: { limit: 500 } } })
				.then((r) => (inventories = r.data?.items ?? []));
		}
	});

	// Inventories the launcher may choose: those they may use, and the template's.
	const inventoryOptions = $derived(
		inventories.filter((i) => i.id === t.inventoryId || can('inventory', i.id, 'use'))
	);

	const commandProblem = $derived.by(() => {
		if (!prompted('command')) return '';
		const e = argvError(values.command);
		if (e) return e;
		return parseArgv(values.command).length === 0 ? 'Enter a command to run' : '';
	});
	// VarsField shows its own problem.
	const problem = $derived(
		(prompted('extraVars') ? varsError(values.extraVars) : '') || commandProblem
	);
	const request = $derived(problem ? null : buildLaunchRequest(t, values));
	const body = $derived(request?.body ?? {});

	// What the job will run with: the template's settings with the launch's over them.
	const inventory = $derived<Ref | null>(
		body.inventoryId
			? {
					id: body.inventoryId,
					name: inventories.find((i) => i.id === body.inventoryId)?.name ?? null
				}
			: refs.inventory
	);
	const runs = $derived(
		t.jobType === 'container'
			? (body.command ?? t.command).join(' ')
			: [t.entrypoint, ...t.arguments].join(' ')
	);
	const extraVars = $derived(fmtVars({ ...t.extraVars, ...body.extraVars }));

	function close() {
		dialog.close();
	}

	async function launch(event: SubmitEvent) {
		event.preventDefault();
		if (problem || !request) {
			error = problem;
			return;
		}
		launching = true;
		error = '';
		const { data, error: err } = await api.POST('/api/v1/job-templates/{jobTemplateId}/launch', {
			params: { path: { jobTemplateId: t.id } },
			body: request.body
		});
		launching = false;
		if (data) goto(`/jobs/${data.id}`);
		else error = errorMessage(err);
	}
</script>

{#snippet name(ref: Ref | null)}
	{#if ref?.name}{ref.name}{:else if ref}<span class="muted">not visible to you</span>{:else}…{/if}
{/snippet}

{#snippet changed(on: boolean)}
	{#if on}<span class="badge accent">changed</span>{/if}
{/snippet}

<!-- A click outside the panel lands on the dialog itself (its backdrop); Esc closes it too. -->
<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_noninteractive_element_interactions -->
<dialog
	bind:this={dialog}
	aria-labelledby="launch-title"
	onclose={() => onclose()}
	onclick={(e) => e.target === dialog && !launching && close()}
>
	<form onsubmit={launch}>
		<header>
			<h2 id="launch-title">Launch {t.name}</h2>
			<span class="badge">{t.jobType}</span>
		</header>

		<div class="body">
			{#if t.allowOverrides.length}
				<h3>Launch settings</h3>
				<p class="muted small">
					These apply to this launch only. They start as the template's settings.
				</p>
				<div class="form-grid">
					{#if prompted('inventory')}
						<label>
							Inventory {@render changed(Boolean(body.inventoryId))}
							<select bind:value={values.inventoryId}>
								{#if !t.inventoryId}<option value="">None</option>{/if}
								{#if t.inventoryId && !inventoryOptions.some((i) => i.id === t.inventoryId)}
									<option value={t.inventoryId}>(the template's, which you can't see)</option>
								{/if}
								{#each inventoryOptions as i (i.id)}
									<option value={i.id}>{i.name}</option>
								{/each}
							</select>
						</label>
					{/if}
					{#if prompted('limit')}
						<label>
							Limit {@render changed(body.limit !== undefined)}
							<input
								bind:value={values.limit}
								maxlength="4096"
								placeholder="web:&db1 (all hosts if blank)"
							/>
						</label>
					{/if}
					{#if prompted('tags')}
						<label>
							Tags {@render changed(body.tags !== undefined)}
							<input bind:value={values.tags} maxlength="4096" placeholder="All tasks" />
						</label>
					{/if}
					{#if prompted('skipTags')}
						<label>
							Skip tags {@render changed(body.skipTags !== undefined)}
							<input bind:value={values.skipTags} maxlength="4096" />
						</label>
					{/if}
				</div>
				{#if prompted('command')}
					<label>
						Command {@render changed(body.command !== undefined)}
						<span class="muted small">one argument per line, not run through a shell</span>
						<textarea
							bind:value={values.command}
							class="code"
							rows={Math.min(12, Math.max(3, values.command.split('\n').length + 1))}
							spellcheck="false"></textarea>
						{#if commandProblem}<span class="error small">{commandProblem}</span>{/if}
					</label>
				{/if}
				{#if prompted('extraVars')}
					<VarsField bind:value={values.extraVars} label="Extra variables">
						{#snippet aside()}{@render changed(body.extraVars !== undefined)}{/snippet}
					</VarsField>
					{#if request?.removedVarKeys.length}
						<p class="muted small note">
							Removing a variable doesn't unset it: the template's value still applies for
							{request.removedVarKeys.map((k) => `"${k}"`).join(', ')}.
						</p>
					{/if}
				{/if}
			{/if}

			<h3>Summary</h3>
			<dl>
				<dt>Execution environment</dt>
				<dd>{@render name(refs.ee)}</dd>
				{#if t.projectId}
					<dt>Project</dt>
					<dd>{@render name(refs.project)}</dd>
				{/if}
				<dt>Runs {@render changed(body.command !== undefined)}</dt>
				<dd><code>{runs}</code></dd>
				<dt>Inventory {@render changed(Boolean(body.inventoryId))}</dt>
				<dd>
					{#if inventory}{@render name(inventory)}{:else}<span class="muted">none</span>{/if}
				</dd>
				{#if t.credentialIds.length}
					<dt>Credentials</dt>
					<dd>
						{#each refs.credentials as c, i (c.id)}{#if i},
							{/if}{@render name(c)}{/each}
					</dd>
				{/if}
				<dt>Limit {@render changed(body.limit !== undefined)}</dt>
				<dd>
					{#if body.limit ?? t.limit}<code>{body.limit ?? t.limit}</code>{:else}<span class="muted"
							>all hosts</span
						>{/if}
				</dd>
				{#if t.jobType === 'ansible'}
					<dt>Tags {@render changed(body.tags !== undefined)}</dt>
					<dd>
						{#if body.tags ?? t.tags}<code>{body.tags ?? t.tags}</code>{:else}<span class="muted"
								>all tasks</span
							>{/if}
					</dd>
					{#if body.skipTags ?? t.skipTags}
						<dt>Skip tags {@render changed(body.skipTags !== undefined)}</dt>
						<dd><code>{body.skipTags ?? t.skipTags}</code></dd>
					{/if}
					{#if t.verbosity}
						<dt>Verbosity</dt>
						<dd><code>-{'v'.repeat(t.verbosity)}</code></dd>
					{/if}
				{/if}
				<dt>Timeout</dt>
				<dd>{fmtSeconds(t.timeoutSeconds)}</dd>
				{#if extraVars}
					<dt>Extra variables {@render changed(body.extraVars !== undefined)}</dt>
					<dd><pre class="code">{extraVars}</pre></dd>
				{/if}
			</dl>
		</div>

		<footer>
			{#if error}<p class="error" role="alert">{error}</p>{/if}
			<div class="form-actions">
				<button type="submit" disabled={launching || Boolean(problem)}>
					{launching ? 'Launching…' : 'Launch'}
				</button>
				<button type="button" class="secondary" onclick={close} disabled={launching}>
					Cancel
				</button>
			</div>
		</footer>
	</form>
</dialog>

<style>
	dialog {
		width: min(720px, calc(100vw - 2rem));
		max-height: calc(100vh - 2rem);
		padding: 0;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--surface);
		color: var(--text);
		box-shadow: 0 10px 40px rgb(0 0 0 / 0.25);
	}

	dialog::backdrop {
		background: rgb(0 0 0 / 0.45);
	}

	form {
		display: flex;
		flex-direction: column;
		max-height: calc(100vh - 2rem);
	}

	header,
	footer {
		padding: 1rem 1.25rem;
	}

	header {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		border-bottom: 1px solid var(--border);
	}

	header h2 {
		margin: 0;
		overflow-wrap: anywhere;
	}

	.body {
		padding: 0.5rem 1.25rem 1rem;
		overflow-y: auto;
	}

	footer {
		border-top: 1px solid var(--border);
	}

	footer .error {
		margin: 0 0 0.75rem;
	}

	footer .form-actions {
		margin: 0;
	}

	h3 {
		font-size: 1rem;
		margin: 1rem 0 0.5rem;
	}

	.note {
		margin-top: -0.5rem;
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
		min-width: 0;
	}

	.badge {
		font-weight: normal;
	}
</style>
