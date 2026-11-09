<script lang="ts">
	import {
		api,
		errorMessage,
		type Credential,
		type ExecutionEnvironment,
		type Inventory,
		type InventorySource,
		type JobStatus,
		type Project,
		type ResourceType
	} from '#lib/api/client.js';
	import { fmtSeconds, fmtTime } from '#lib/format.js';
	import { can } from '#lib/permissions.svelte.js';
	import Schedules from './Schedules.svelte';
	import StatusBadge from './StatusBadge.svelte';

	/**
	 * Where an inventory is synced from: shows the source and its last sync,
	 * syncs on request, and lets the inventory's admins set, change or
	 * remove the source. onchanged runs when the inventory's hosts and
	 * groups may have changed (a sync finished, the source was set or
	 * removed).
	 */
	let {
		inventory,
		canAdmin,
		onchanged
	}: { inventory: Inventory; canAdmin: boolean; onchanged: () => void } = $props();

	// Credential kinds a sync can use; see the API's setInventorySource.
	const syncKinds = ['generic', 'vault', 'api_token'];
	const active: JobStatus[] = ['pending', 'waiting', 'running'];

	let source = $state<InventorySource | null>(null);
	let loaded = $state(false);
	let editing = $state(false);
	let error = $state('');
	let syncing = $state(false);

	// Form.
	let projectId = $state('');
	let sourcePath = $state('');
	let eeId = $state('');
	let credentialIds = $state<string[]>([]);
	let timeout = $state<number | null>(600);
	let verbosity = $state(0);
	let saving = $state(false);
	let formError = $state('');

	let ees = $state<ExecutionEnvironment[]>([]);
	let projects = $state<Project[]>([]);
	let credentials = $state<Credential[]>([]);

	async function load(iid: string) {
		const { data, response } = await api.GET('/api/v1/inventories/{inventoryId}/source', {
			params: { path: { inventoryId: iid } }
		});
		if (data) source = data;
		else if (response.status === 404) source = null;
		loaded = true;
	}

	$effect(() => {
		const iid = inventory.id;
		loaded = false;
		editing = false;
		load(iid);
	});

	// Names for the source's credentials.
	$effect(() => {
		if (source?.credentialIds.length && credentials.length === 0) {
			api
				.GET('/api/v1/credentials', { params: { query: { limit: 500 } } })
				.then(({ data }) => (credentials = data?.items ?? []));
		}
	});

	// While a sync runs, check on it; refresh the inventory when it ends.
	$effect(() => {
		const status = source?.lastJobStatus;
		if (!status || !active.includes(status)) return;
		const iid = inventory.id;
		const timer = setInterval(async () => {
			await load(iid);
			if (source?.lastJobStatus && !active.includes(source.lastJobStatus)) onchanged();
		}, 2000);
		return () => clearInterval(timer);
	});

	async function openForm() {
		const s = source;
		projectId = s?.projectId ?? '';
		sourcePath = s?.sourcePath ?? '';
		eeId = s?.executionEnvironmentId ?? '';
		credentialIds = [...(s?.credentialIds ?? [])];
		timeout = s?.timeoutSeconds ?? 600;
		verbosity = s?.verbosity ?? 0;
		formError = '';
		editing = true;
		const query = { params: { query: { limit: 500 } } };
		const [e, p, c] = await Promise.all([
			api.GET('/api/v1/execution-environments', query),
			api.GET('/api/v1/projects', query),
			api.GET('/api/v1/credentials', query)
		]);
		ees = e.data?.items ?? [];
		projects = p.data?.items ?? [];
		credentials = c.data?.items ?? [];
		if (!eeId) eeId = (ees.find((x) => /ansible/i.test(x.name)) ?? ees[0])?.id ?? '';
	}

	/** What a picker offers: the ones the user may use, and those already chosen. */
	function usable<T extends { id: string }>(type: ResourceType, items: T[], keep: string[]): T[] {
		return items.filter((x) => keep.includes(x.id) || can(type, x.id, 'use'));
	}

	const projectOptions = $derived(usable('project', projects, [source?.projectId ?? '']));
	const credentialOptions = $derived(
		usable('credential', credentials, source?.credentialIds ?? []).filter((c) =>
			syncKinds.includes(c.kind)
		)
	);
	// Credentials the source has that the user can't see are kept as they are.
	const hiddenCredentialIds = $derived(
		(source?.credentialIds ?? []).filter((id) => !credentials.some((c) => c.id === id))
	);
	const credentialName = $derived(new Map(credentials.map((c) => [c.id, c.name])));

	async function save(event: SubmitEvent) {
		event.preventDefault();
		saving = true;
		formError = '';
		const { data, error: err } = await api.PUT('/api/v1/inventories/{inventoryId}/source', {
			params: { path: { inventoryId: inventory.id } },
			body: {
				projectId,
				sourcePath: sourcePath.trim(),
				executionEnvironmentId: eeId,
				credentialIds: [...credentialIds, ...hiddenCredentialIds],
				timeoutSeconds: timeout ?? 600,
				verbosity
			}
		});
		saving = false;
		if (!data) {
			formError = errorMessage(err);
			return;
		}
		const first = source === null;
		source = data;
		editing = false;
		onchanged();
		if (first) await sync();
	}

	async function remove() {
		if (
			!confirm(
				'Stop syncing this inventory? Its hosts and groups stay as they are and can be edited by hand.'
			)
		)
			return;
		error = '';
		const { error: err } = await api.DELETE('/api/v1/inventories/{inventoryId}/source', {
			params: { path: { inventoryId: inventory.id } }
		});
		if (err) {
			error = errorMessage(err);
			return;
		}
		source = null;
		onchanged();
	}

	async function sync() {
		syncing = true;
		error = '';
		const { error: err } = await api.POST('/api/v1/inventories/{inventoryId}/sync', {
			params: { path: { inventoryId: inventory.id } }
		});
		syncing = false;
		if (err) error = errorMessage(err);
		await load(inventory.id);
	}

	const running = $derived(Boolean(source?.lastJobStatus && active.includes(source.lastJobStatus)));
</script>

<section class="card">
	<div class="page-header">
		<h2>Source</h2>
		<span class="spacer"></span>
		{#if canAdmin && source && !editing}
			<button onclick={sync} disabled={syncing || running}>
				{running ? 'Syncing…' : 'Sync now'}
			</button>
			<button class="secondary" onclick={openForm}>Edit source</button>
			<button class="danger" onclick={remove}>Remove source</button>
		{/if}
	</div>
	{#if error}<p class="error" role="alert">{error}</p>{/if}

	{#if !loaded}
		<p class="muted">Loading…</p>
	{:else if editing}
		{#if formError}<p class="error" role="alert">{formError}</p>{/if}
		<form onsubmit={save}>
			<div class="form-grid">
				<label>
					Project
					<select bind:value={projectId} required>
						<option value="" disabled>Choose a project</option>
						{#each projectOptions as p (p.id)}
							<option value={p.id}>{p.name}</option>
						{/each}
					</select>
				</label>
				<label>
					Inventory file
					<input
						bind:value={sourcePath}
						required
						maxlength="1024"
						placeholder="inventory_akips.yml"
						spellcheck="false"
					/>
				</label>
				<label>
					Execution environment
					<select bind:value={eeId} required>
						<option value="" disabled>Choose one</option>
						{#each ees as e (e.id)}
							<option value={e.id}>{e.name}</option>
						{/each}
					</select>
				</label>
				<label>
					Timeout (seconds)
					<input type="number" min="1" max="86400" bind:value={timeout} />
				</label>
				<label>
					Verbosity
					<select bind:value={verbosity}>
						{#each [0, 1, 2, 3, 4, 5] as v (v)}
							<option value={v}>{v === 0 ? 'Normal' : '-' + 'v'.repeat(v)}</option>
						{/each}
					</select>
				</label>
			</div>
			<fieldset class="creds">
				<legend>Credentials</legend>
				{#if credentialOptions.length === 0}
					<p class="muted small">No generic, vault or API token credentials you can use.</p>
				{/if}
				{#each credentialOptions as c (c.id)}
					<label class="inline">
						<input type="checkbox" value={c.id} bind:group={credentialIds} />
						{c.name} <span class="muted small">{c.kind}</span>
					</label>
				{/each}
				{#if hiddenCredentialIds.length}
					<p class="muted small">
						Also {hiddenCredentialIds.length} credential(s) you can't see, which are kept.
					</p>
				{/if}
			</fieldset>
			<p class="muted small">
				The file (or directory) is read with <code>ansible-inventory</code> in the project's
				checkout, after the collections in its <code>collections/requirements.yml</code> are
				installed. Each value of a generic credential is set as an environment variable of the same
				name, which is how inventory plugins usually take their connection settings (e.g.
				<code>AKIPS_HOST</code>, <code>AKIPS_PASSWORD</code>). Every sync replaces the hosts and
				groups, which can't be edited by hand while the inventory has a source.
			</p>
			<div class="form-actions">
				<button type="submit" disabled={saving}>
					{saving ? 'Saving…' : source ? 'Save source' : 'Save and sync'}
				</button>
				<button type="button" class="secondary" onclick={() => (editing = false)}>Cancel</button>
			</div>
		</form>
	{:else if source}
		<dl>
			<dt>Synced from</dt>
			<dd>
				<code>{source.sourcePath}</code> in
				<a href="/projects/{source.projectId}">{source.projectName}</a>
			</dd>
			<dt>Runs in</dt>
			<dd>
				{source.executionEnvironmentName}
				<span class="muted small"
					>· timeout {fmtSeconds(source.timeoutSeconds)}{source.verbosity
						? ` · -${'v'.repeat(source.verbosity)}`
						: ''}</span
				>
			</dd>
			<dt>Credentials</dt>
			<dd>
				{source.credentialIds.length
					? source.credentialIds.map((id) => credentialName.get(id) ?? 'credential').join(', ')
					: 'none'}
			</dd>
			<dt>Last sync</dt>
			<dd>
				{#if source.lastJobId}
					<a href="/jobs/{source.lastJobId}">job</a>
					{#if source.lastJobStatus}<StatusBadge status={source.lastJobStatus} />{/if}
				{/if}
				{source.lastSyncedAt
					? `· hosts and groups replaced ${fmtTime(source.lastSyncedAt)}`
					: '· never synced'}
			</dd>
		</dl>
		{#if canAdmin}
			<h3>Scheduled syncs</h3>
			<Schedules inventoryId={inventory.id} empty="Synced only on request." />
		{/if}
	{:else}
		<p class="muted">
			Hosts and groups are edited by hand.
			{#if canAdmin}You can sync them from an inventory file or plugin configuration in a project
				instead.{/if}
		</p>
		{#if canAdmin}<button onclick={openForm}>Sync from a project</button>{/if}
	{/if}
</section>

<style>
	.page-header h2 {
		margin: 0;
	}

	section .page-header {
		margin-bottom: 0.75rem;
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
	}

	.creds {
		border: none;
		padding: 0;
		margin: 0.75rem 0;
	}

	.creds legend {
		font-weight: 600;
		margin-bottom: 0.25rem;
	}
</style>
