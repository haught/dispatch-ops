<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import {
		api,
		errorMessage,
		listCredentialKinds,
		listTemplates,
		type Credential,
		type CredentialKindInfo,
		type ExecutionEnvironment,
		type JobTemplate,
		type Project
	} from '#lib/api/client.js';
	import CredentialInputs from '#lib/components/CredentialInputs.svelte';
	import ResourceAccess from '#lib/components/ResourceAccess.svelte';
	import UsedBy from '#lib/components/UsedBy.svelte';
	import { fmtTime } from '#lib/format.js';
	import { can, loadMyGrants } from '#lib/permissions.svelte.js';

	const id = $derived(page.params.id ?? '');

	let credential = $state<Credential | null>(null);
	let kinds = $state<CredentialKindInfo[]>([]);
	let templates = $state<JobTemplate[]>([]);
	let projects = $state<Project[]>([]);
	let images = $state<ExecutionEnvironment[]>([]);
	let error = $state('');

	let name = $state('');
	let description = $state('');
	let inputsEl = $state<CredentialInputs>();
	let saving = $state(false);
	let message = $state('');
	let formError = $state('');

	const kindInfo = $derived(kinds.find((k) => k.kind === credential?.kind));
	const canAdmin = $derived(can('credential', id, 'admin'));

	function fieldLabel(field: string): string {
		return kindInfo?.fields.find((f) => f.name === field)?.label ?? field;
	}

	async function load(cid: string) {
		const { data, error: err } = await api.GET('/api/v1/credentials/{credentialId}', {
			params: { path: { credentialId: cid } }
		});
		if (!data) {
			error = errorMessage(err);
			return;
		}
		credential = data;
		name = data.name;
		description = data.description;
	}

	async function loadUses(cid: string) {
		const [t, p, e] = await Promise.all([
			listTemplates(),
			api.GET('/api/v1/projects', { params: { query: { limit: 500 } } }),
			api.GET('/api/v1/execution-environments', { params: { query: { limit: 500 } } })
		]);
		templates = t.filter((x) => x.credentialIds.includes(cid));
		projects = (p.data?.items ?? []).filter((x) => x.credentialId === cid);
		images = (e.data?.items ?? []).filter((x) => x.registryCredentialId === cid);
	}

	$effect(() => {
		const cid = id;
		if (!cid) return;
		credential = null;
		error = '';
		load(cid);
		loadUses(cid);
	});

	$effect(() => {
		listCredentialKinds().then((k) => (kinds = k));
		loadMyGrants();
	});

	async function save(event: SubmitEvent) {
		event.preventDefault();
		if (!credential || !inputsEl) return;
		saving = true;
		formError = message = '';
		const inputs = inputsEl.inputs();
		const { data, error: err } = await api.PATCH('/api/v1/credentials/{credentialId}', {
			params: { path: { credentialId: credential.id } },
			body: {
				name,
				description,
				inputs: Object.keys(inputs).length ? inputs : undefined
			}
		});
		saving = false;
		if (!data) {
			formError = errorMessage(err);
			return;
		}
		credential = data;
		message = 'Saved.';
	}

	async function remove() {
		if (!credential || !confirm(`Delete the credential ${credential.name}?`)) return;
		error = '';
		const { error: err } = await api.DELETE('/api/v1/credentials/{credentialId}', {
			params: { path: { credentialId: credential.id } }
		});
		if (err) error = errorMessage(err);
		else goto('/credentials');
	}
</script>

{#if credential}
	<div class="page-header">
		<h1>{credential.name}</h1>
		<span class="badge">{kindInfo?.label ?? credential.kind}</span>
		<span class="spacer"></span>
		{#if canAdmin}<button class="danger" onclick={remove}>Delete</button>{/if}
	</div>
	{#if credential.description}<p class="muted">{credential.description}</p>{/if}
	{#if error}<p class="error" role="alert">{error}</p>{/if}

	<section class="card">
		<dl class="details">
			{#each Object.entries(credential.publicInputs) as [field, value] (field)}
				<dt>{fieldLabel(field)}</dt>
				<dd><code>{value}</code></dd>
			{/each}
			<dt>Secrets set</dt>
			<dd>
				{credential.secretFields.length
					? credential.secretFields.map(fieldLabel).join(', ')
					: 'none'}
			</dd>
			<dt>Created</dt>
			<dd>{fmtTime(credential.createdAt)}</dd>
			<dt>Updated</dt>
			<dd>{fmtTime(credential.updatedAt)}</dd>
		</dl>
	</section>

	{#if canAdmin && kindInfo}
		<section class="card">
			<h2>Edit</h2>
			{#if formError}<p class="error" role="alert">{formError}</p>{/if}
			<form onsubmit={save}>
				<div class="form-grid">
					<label>Name <input bind:value={name} maxlength="255" required /></label>
					<label>Description <input bind:value={description} maxlength="4096" /></label>
				</div>
				{#key credential.updatedAt}
					<CredentialInputs bind:this={inputsEl} kind={kindInfo} {credential} />
				{/key}
				<div class="form-actions">
					<button type="submit" disabled={saving}>{saving ? 'Saving…' : 'Save'}</button>
					{#if message}<span class="muted">{message}</span>{/if}
				</div>
			</form>
		</section>
	{/if}

	<UsedBy {templates} what="credential" />
	{#if projects.length || images.length}
		<section class="card">
			<h2>Also used by</h2>
			<ul>
				{#each projects as p (p.id)}
					<li>
						Project <a href="/projects/{p.id}">{p.name}</a>
						<span class="muted small">to clone</span>
					</li>
				{/each}
				{#each images as e (e.id)}
					<li>
						Execution environment <a href="/execution-environments">{e.name}</a>
						<span class="muted small">to pull <code>{e.image}</code></span>
					</li>
				{/each}
			</ul>
		</section>
	{/if}

	<ResourceAccess type="credential" id={credential.id} />
{:else if error}
	<p class="error" role="alert">{error}</p>
	<p><a href="/credentials">Back to credentials</a></p>
{:else}
	<p class="muted">Loading…</p>
{/if}

<style>
	ul {
		margin: 0;
		padding-left: 1.25rem;
	}
</style>
