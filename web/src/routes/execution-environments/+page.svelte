<script lang="ts">
	import {
		api,
		errorMessage,
		listTemplates,
		pullPolicies,
		type Credential,
		type ExecutionEnvironment,
		type JobTemplate,
		type PullPolicy
	} from '#lib/api/client.js';
	import { session } from '#lib/session.svelte.js';

	let images = $state<ExecutionEnvironment[]>([]);
	let total = $state(0);
	let credentials = $state<Credential[]>([]);
	let templates = $state<JobTemplate[]>([]);
	let search = $state('');
	let loading = $state(true);
	let error = $state('');

	const isAdmin = $derived(Boolean(session.user?.isAdmin));

	// The form creates an execution environment, or edits the one in editing.
	let editing = $state<ExecutionEnvironment | null>(null);
	let name = $state('');
	let description = $state('');
	let image = $state('');
	let pullPolicy = $state<PullPolicy>('IfNotPresent');
	let registryCredentialId = $state('');
	let formError = $state('');
	let saving = $state(false);

	async function load() {
		const { data, error: err } = await api.GET('/api/v1/execution-environments', {
			params: { query: { limit: 500, search: search || undefined } }
		});
		loading = false;
		if (data) {
			images = data.items;
			total = data.total;
		} else {
			error = errorMessage(err);
		}
	}

	$effect(() => {
		void search;
		load();
	});

	$effect(() => {
		listTemplates().then((t) => (templates = t));
		api
			.GET('/api/v1/credentials', { params: { query: { kind: 'registry', limit: 500 } } })
			.then(({ data }) => (credentials = data?.items ?? []));
	});

	function usedBy(e: ExecutionEnvironment): number {
		return templates.filter((t) => t.executionEnvironmentId === e.id).length;
	}

	function credentialName(id: string): string {
		return credentials.find((c) => c.id === id)?.name ?? 'a credential';
	}

	function reset() {
		editing = null;
		name = description = image = registryCredentialId = '';
		pullPolicy = 'IfNotPresent';
		formError = '';
	}

	function edit(e: ExecutionEnvironment) {
		reset();
		editing = e;
		name = e.name;
		description = e.description;
		image = e.image;
		pullPolicy = e.pullPolicy;
		registryCredentialId = e.registryCredentialId ?? '';
	}

	async function save(event: SubmitEvent) {
		event.preventDefault();
		saving = true;
		formError = '';
		const { error: err } = editing
			? await api.PATCH('/api/v1/execution-environments/{executionEnvironmentId}', {
					params: { path: { executionEnvironmentId: editing.id } },
					body: {
						name,
						description,
						image: image.trim(),
						pullPolicy,
						registryCredentialId: registryCredentialId || null
					}
				})
			: await api.POST('/api/v1/execution-environments', {
					body: {
						name,
						description,
						image: image.trim(),
						pullPolicy,
						registryCredentialId: registryCredentialId || undefined
					}
				});
		saving = false;
		if (err) {
			formError = errorMessage(err);
			return;
		}
		reset();
		await load();
	}

	async function remove(e: ExecutionEnvironment) {
		if (!confirm(`Delete the execution environment ${e.name}?`)) return;
		error = '';
		const { error: err } = await api.DELETE(
			'/api/v1/execution-environments/{executionEnvironmentId}',
			{ params: { path: { executionEnvironmentId: e.id } } }
		);
		if (err) error = errorMessage(err);
		if (editing?.id === e.id) reset();
		await load();
	}
</script>

<h1>Execution environments</h1>
<p class="muted">
	The container images jobs run in. Each is built with the Dispatch Ops runner as its entrypoint,
	plus the tools its jobs need. Job templates choose one.
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
	{:else if images.length === 0}
		<p class="muted">No execution environments{search ? ' match' : ' yet'}.</p>
	{:else}
		<div class="table-wrap">
			<table>
				<thead>
					<tr>
						<th>Name</th>
						<th>Image</th>
						<th>Pull</th>
						<th>Templates</th>
						<th></th>
					</tr>
				</thead>
				<tbody>
					{#each images as e (e.id)}
						<tr class:editing={editing?.id === e.id}>
							<td>
								{e.name}
								{#if e.description}<div class="muted small">{e.description}</div>{/if}
							</td>
							<td class="small">
								<code>{e.image}</code>
								{#if e.registryCredentialId}
									<div class="muted">
										pulled with
										<a href="/credentials/{e.registryCredentialId}"
											>{credentialName(e.registryCredentialId)}</a
										>
									</div>
								{/if}
							</td>
							<td class="small">{e.pullPolicy}</td>
							<td>{usedBy(e)}</td>
							<td>
								{#if isAdmin}
									<div class="row-actions">
										<button class="secondary" onclick={() => edit(e)}>Edit</button>
										<button class="danger" onclick={() => remove(e)}>Delete</button>
									</div>
								{/if}
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
		{#if total > images.length}
			<p class="muted">Showing {images.length} of {total}. Narrow the search to see others.</p>
		{/if}
	{/if}
</section>

{#if isAdmin}
	<section class="card">
		<h2>{editing ? `Edit ${editing.name}` : 'New execution environment'}</h2>
		{#if formError}<p class="error" role="alert">{formError}</p>{/if}
		<form onsubmit={save}>
			<div class="form-grid">
				<label>Name <input bind:value={name} maxlength="255" required /></label>
				<label>Description <input bind:value={description} maxlength="4096" /></label>
			</div>
			<label>
				Image
				<input
					bind:value={image}
					required
					maxlength="512"
					pattern="\S+"
					placeholder="ghcr.io/acme/ee-ansible:1.2 or …@sha256:…"
				/>
			</label>
			<div class="form-grid">
				<label>
					Pull policy
					<select bind:value={pullPolicy}>
						{#each pullPolicies as p (p)}
							<option value={p}>{p}</option>
						{/each}
					</select>
				</label>
				<label>
					Registry credential
					<select bind:value={registryCredentialId}>
						<option value="">None (public image)</option>
						{#if editing?.registryCredentialId && !credentials.some((c) => c.id === editing?.registryCredentialId)}
							<option value={editing.registryCredentialId}>(a credential you can't see)</option>
						{/if}
						{#each credentials as c (c.id)}
							<option value={c.id}>{c.name}</option>
						{/each}
					</select>
				</label>
			</div>
			<p class="muted small">
				Images from private registries need a <a href="/credentials"
					>container registry credential</a
				>; it becomes the job pod's image pull secret.
			</p>
			<div class="form-actions">
				<button type="submit" disabled={saving}>
					{editing ? 'Save' : 'Add execution environment'}
				</button>
				{#if editing}<button type="button" class="secondary" onclick={reset}>Cancel</button>{/if}
			</div>
		</form>
	</section>
{/if}

<style>
	.search {
		max-width: 320px;
	}

	tr.editing td {
		background: var(--bg);
	}
</style>
