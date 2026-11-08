<script lang="ts">
	import { untrack } from 'svelte';
	import {
		api,
		errorMessage,
		type Credential,
		type ExecutionEnvironment,
		type Inventory,
		type JobTemplate,
		type JobType,
		type OverridableField,
		type Project,
		type ResourceType
	} from '#lib/api/client.js';
	import {
		argvError,
		fmtArgv,
		fmtSeconds,
		fmtVars,
		parseArgv,
		parseVars,
		varsError
	} from '#lib/format.js';
	import { can } from '#lib/permissions.svelte.js';
	import VarsField from './VarsField.svelte';

	/**
	 * Creates a job template, or edits one (its job type can't change). The
	 * page loads the signed-in user's grants: pickers offer the projects,
	 * inventories and credentials they may use, plus whatever the template
	 * already has.
	 */
	let {
		template = null,
		onsaved,
		oncancel
	}: {
		template?: JobTemplate | null;
		onsaved: (t: JobTemplate) => void;
		oncancel?: () => void;
	} = $props();

	const jobTypes: { type: JobType; label: string; help: string }[] = [
		{ type: 'ansible', label: 'Ansible', help: 'Runs a playbook from a project' },
		{ type: 'script', label: 'Script', help: 'Runs a script from a project' },
		{ type: 'container', label: 'Container', help: 'Runs a command in the image' }
	];

	const overridable: { field: OverridableField; label: string; types: JobType[] }[] = [
		{ field: 'inventory', label: 'Inventory', types: ['ansible', 'script', 'container'] },
		{ field: 'extraVars', label: 'Extra variables', types: ['ansible', 'script', 'container'] },
		{ field: 'limit', label: 'Limit', types: ['ansible', 'script', 'container'] },
		{ field: 'command', label: 'Command', types: ['container'] },
		{ field: 'tags', label: 'Tags', types: ['ansible'] },
		{ field: 'skipTags', label: 'Skip tags', types: ['ansible'] }
	];

	const t = untrack(() => template);
	let jobType = $state<JobType>(t?.jobType ?? 'ansible');
	let name = $state(t?.name ?? '');
	let description = $state(t?.description ?? '');
	let eeId = $state(t?.executionEnvironmentId ?? '');
	let projectId = $state(t?.projectId ?? '');
	let entrypoint = $state(t?.entrypoint ?? '');
	let command = $state(fmtArgv(t?.command ?? []));
	let args = $state(fmtArgv(t?.arguments ?? []));
	let inventoryId = $state(t?.inventoryId ?? '');
	// The checked credentials among those offered; see credentialOptions.
	let credentialIds = $state<string[]>([...(t?.credentialIds ?? [])]);
	let vars = $state(fmtVars(t?.extraVars));
	let limit = $state(t?.limit ?? '');
	let tags = $state(t?.tags ?? '');
	let skipTags = $state(t?.skipTags ?? '');
	let verbosity = $state(t?.verbosity ?? 0);
	let timeout = $state<number | null>(t?.timeoutSeconds ?? 3600);
	let allowOverrides = $state<OverridableField[]>([...(t?.allowOverrides ?? [])]);

	let ees = $state<ExecutionEnvironment[]>([]);
	let projects = $state<Project[]>([]);
	let inventories = $state<Inventory[]>([]);
	let credentials = $state<Credential[]>([]);
	let loaded = $state(false);
	let saving = $state(false);
	let error = $state('');

	$effect(() => {
		const query = { params: { query: { limit: 500 } } };
		Promise.all([
			api.GET('/api/v1/execution-environments', query),
			api.GET('/api/v1/projects', query),
			api.GET('/api/v1/inventories', query),
			api.GET('/api/v1/credentials', query)
		]).then(([e, p, i, c]) => {
			ees = e.data?.items ?? [];
			projects = p.data?.items ?? [];
			inventories = i.data?.items ?? [];
			credentials = c.data?.items ?? [];
			// Pick the only execution environment there is.
			if (!eeId && ees.length === 1) eeId = ees[0].id;
			loaded = true;
		});
	});

	/** What a picker offers: the ones the user may use, and the one already chosen. */
	function usable<T extends { id: string }>(type: ResourceType, items: T[], keep: string[]): T[] {
		return items.filter((x) => keep.includes(x.id) || can(type, x.id, 'use'));
	}

	const projectOptions = $derived(usable('project', projects, [t?.projectId ?? '']));
	const inventoryOptions = $derived(usable('inventory', inventories, [t?.inventoryId ?? '']));
	const credentialOptions = $derived(usable('credential', credentials, t?.credentialIds ?? []));
	// Credentials the template has that the user can't see are kept as they are.
	const hiddenCredentialIds = $derived(
		loaded ? (t?.credentialIds ?? []).filter((id) => !credentials.some((c) => c.id === id)) : []
	);

	const usesProject = $derived(jobType !== 'container');
	const what = $derived(jobType === 'ansible' ? 'Playbook' : 'Script');
	const overrideOptions = $derived(overridable.filter((o) => o.types.includes(jobType)));
	const commandProblem = $derived(jobType === 'container' ? argvError(command) : '');
	const argsProblem = $derived(jobType === 'script' ? argvError(args) : '');

	function check(): string {
		if (commandProblem || argsProblem) return commandProblem || argsProblem;
		if (jobType === 'container' && parseArgv(command).length === 0) return 'Enter a command to run';
		return varsError(vars);
	}

	async function save(event: SubmitEvent) {
		event.preventDefault();
		error = check();
		if (error) return;
		saving = true;
		const body = {
			name: name.trim(),
			description,
			executionEnvironmentId: eeId,
			credentialIds: [...credentialIds, ...hiddenCredentialIds],
			extraVars: parseVars(vars),
			limit: limit.trim(),
			timeoutSeconds: timeout ?? 3600,
			allowOverrides: allowOverrides.filter((f) => overrideOptions.some((o) => o.field === f)),
			...(usesProject ? { projectId, entrypoint: entrypoint.trim() } : {}),
			...(jobType === 'container' ? { command: parseArgv(command) } : {}),
			...(jobType === 'script' ? { arguments: parseArgv(args) } : {}),
			...(jobType === 'ansible' ? { tags: tags.trim(), skipTags: skipTags.trim(), verbosity } : {})
		};
		const { data, error: err } = template
			? await api.PATCH('/api/v1/job-templates/{jobTemplateId}', {
					params: { path: { jobTemplateId: template.id } },
					body: { ...body, inventoryId: inventoryId || null }
				})
			: await api.POST('/api/v1/job-templates', {
					body: { ...body, jobType, inventoryId: inventoryId || undefined }
				});
		saving = false;
		if (data) onsaved(data);
		else error = errorMessage(err);
	}
</script>

{#snippet hidden(chosen: string, options: { id: string }[], label: string)}
	{#if loaded && chosen && !options.some((o) => o.id === chosen)}
		<option value={chosen}>({label} you can't see)</option>
	{/if}
{/snippet}

<form onsubmit={save}>
	{#if error}<p class="error" role="alert">{error}</p>{/if}

	<div class="field">
		Job type
		{#if template}<span class="muted small">can't be changed</span>{/if}
		<div class="types" role="radiogroup">
			{#each jobTypes as jt (jt.type)}
				<label class:chosen={jobType === jt.type}>
					<input
						type="radio"
						name="jobType"
						bind:group={jobType}
						value={jt.type}
						disabled={Boolean(template)}
					/>
					<span>
						<strong>{jt.label}</strong>
						<span class="muted small">{jt.help}</span>
					</span>
				</label>
			{/each}
		</div>
	</div>

	<div class="form-grid">
		<label>Name <input bind:value={name} maxlength="255" required /></label>
		<label>Description <input bind:value={description} maxlength="4096" /></label>
	</div>

	<h3>What to run</h3>
	<div class="form-grid">
		<label>
			Execution environment
			<select bind:value={eeId} required>
				<option value="" disabled>Choose an image…</option>
				{@render hidden(eeId, ees, 'an execution environment')}
				{#each ees as e (e.id)}
					<option value={e.id}>{e.name} — {e.image}</option>
				{/each}
			</select>
		</label>
		{#if usesProject}
			<label>
				Project
				<select bind:value={projectId} required>
					<option value="" disabled>Choose a project…</option>
					{@render hidden(projectId, projectOptions, 'a project')}
					{#each projectOptions as p (p.id)}
						<option value={p.id}>{p.name}</option>
					{/each}
				</select>
			</label>
			<label>
				{what} path
				<input
					bind:value={entrypoint}
					required
					maxlength="1024"
					placeholder={jobType === 'ansible' ? 'playbooks/site.yml' : 'scripts/backup.sh'}
				/>
			</label>
		{/if}
	</div>
	{#if loaded && ees.length === 0}
		<p class="muted small">
			No <a href="/execution-environments">execution environments</a> yet; a system administrator registers
			them.
		</p>
	{/if}
	{#if loaded && usesProject && projectOptions.length === 0}
		<p class="muted small">
			No <a href="/projects">projects</a> you can use. {jobType === 'ansible'
				? 'Playbooks'
				: 'Scripts'} come from a project's git repository.
		</p>
	{/if}

	{#if jobType === 'container'}
		<label>
			Command <span class="muted small">one argument per line, not run through a shell</span>
			<textarea
				bind:value={command}
				class="code"
				rows={Math.min(12, Math.max(3, command.split('\n').length + 1))}
				spellcheck="false"
				required
				placeholder={'bash\n-c\necho hello from $(hostname)'}></textarea>
			{#if commandProblem}<span class="error small">{commandProblem}</span>{/if}
		</label>
	{:else if jobType === 'script'}
		<label>
			Arguments <span class="muted small">one per line</span>
			<textarea
				bind:value={args}
				class="code"
				rows={Math.min(12, Math.max(2, args.split('\n').length + 1))}
				spellcheck="false"
				placeholder={'--verbose\n--target=prod'}></textarea>
			{#if argsProblem}<span class="error small">{argsProblem}</span>{/if}
		</label>
	{/if}
	{#if jobType !== 'ansible'}
		<p class="muted small hint">
			For an argument with line breaks, enter the whole list as a JSON array instead, e.g.
			<code>["bash", "-c", "set -e\necho hi"]</code>.
		</p>
	{/if}

	<h3>Hosts and secrets</h3>
	<div class="form-grid">
		<label>
			Inventory
			<select bind:value={inventoryId}>
				<option value="">None</option>
				{@render hidden(inventoryId, inventoryOptions, 'an inventory')}
				{#each inventoryOptions as i (i.id)}
					<option value={i.id}>{i.name}</option>
				{/each}
			</select>
		</label>
		<label>
			Limit
			<input bind:value={limit} maxlength="4096" placeholder="web:&db1 (all hosts if blank)" />
		</label>
	</div>
	<div class="field">
		Credentials
		{#if jobType === 'ansible'}
			<span class="muted small">at most one SSH or username/password credential</span>
		{/if}
		{#if credentialOptions.length}
			<div class="checks">
				{#each credentialOptions as c (c.id)}
					<label>
						<input type="checkbox" bind:group={credentialIds} value={c.id} />
						{c.name} <span class="muted small">{c.kind}</span>
					</label>
				{/each}
			</div>
		{:else if loaded}
			<p class="muted small">No <a href="/credentials">credentials</a> you can use.</p>
		{/if}
		{#if hiddenCredentialIds.length}
			<p class="muted small">
				Also keeps {hiddenCredentialIds.length === 1
					? 'a credential'
					: `${hiddenCredentialIds.length} credentials`} you can't see.
			</p>
		{/if}
	</div>
	<VarsField bind:value={vars} label="Extra variables" />

	<h3>Options</h3>
	<div class="form-grid">
		{#if jobType === 'ansible'}
			<label>Tags <input bind:value={tags} maxlength="4096" placeholder="All tasks" /></label>
			<label>Skip tags <input bind:value={skipTags} maxlength="4096" /></label>
			<label>
				Verbosity
				<select bind:value={verbosity}>
					<option value={0}>Normal</option>
					{#each [1, 2, 3, 4, 5] as v (v)}
						<option value={v}>{v} (-{'v'.repeat(v)})</option>
					{/each}
				</select>
			</label>
		{/if}
		<label>
			Timeout (seconds)
			<input type="number" bind:value={timeout} min="1" max="604800" required />
		</label>
	</div>
	{#if timeout && timeout > 0}
		<p class="muted small hint">Jobs that run longer than {fmtSeconds(timeout)} are stopped.</p>
	{/if}

	<div class="field">
		Can be overridden at launch
		<div class="checks">
			{#each overrideOptions as o (o.field)}
				<label
					><input type="checkbox" bind:group={allowOverrides} value={o.field} /> {o.label}</label
				>
			{/each}
		</div>
	</div>

	<div class="form-actions">
		<button type="submit" disabled={saving || !loaded}>
			{saving ? 'Saving…' : template ? 'Save' : 'Create job template'}
		</button>
		{#if oncancel}
			<button type="button" class="secondary" onclick={oncancel}>Cancel</button>
		{/if}
		{#if template}
			<span class="muted small">Jobs already launched keep the settings they started with.</span>
		{/if}
	</div>
</form>

<style>
	h3 {
		font-size: 1rem;
		margin: 1.25rem 0 0.5rem;
	}

	.field {
		font-weight: 500;
		margin-bottom: 0.75rem;
	}

	.hint {
		margin-top: -0.25rem;
	}

	.types {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
		gap: 0.5rem;
		margin-top: 0.25rem;
	}

	.types label {
		display: flex;
		gap: 0.5rem;
		align-items: flex-start;
		margin: 0;
		padding: 0.6rem 0.75rem;
		border: 1px solid var(--border);
		border-radius: 6px;
		font-weight: normal;
		cursor: pointer;
	}

	.types label.chosen {
		border-color: var(--accent);
	}

	.types label:has(input:disabled) {
		cursor: default;
	}

	.types label:has(input:disabled):not(.chosen) {
		opacity: 0.5;
	}

	.types input {
		margin-top: 0.2rem;
	}

	.types span {
		display: flex;
		flex-direction: column;
	}
</style>
