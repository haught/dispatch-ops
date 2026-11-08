<script lang="ts">
	import { goto } from '$app/navigation';
	import {
		api,
		errorMessage,
		listCredentialKinds,
		type Credential,
		type CredentialKind,
		type CredentialKindInfo
	} from '#lib/api/client.js';
	import CredentialInputs from '#lib/components/CredentialInputs.svelte';
	import { fmtTime } from '#lib/format.js';
	import { canCreate, loadMyGrants } from '#lib/permissions.svelte.js';

	let credentials = $state<Credential[]>([]);
	let total = $state(0);
	let kinds = $state<CredentialKindInfo[]>([]);
	let search = $state('');
	let kindFilter = $state<CredentialKind | ''>('');
	let loading = $state(true);
	let error = $state('');

	let name = $state('');
	let description = $state('');
	let kind = $state<CredentialKind>('ssh');
	let inputsEl = $state<CredentialInputs>();
	let formError = $state('');
	let saving = $state(false);

	const kindInfo = $derived(kinds.find((k) => k.kind === kind));
	const mayCreate = $derived(canCreate('credential'));

	async function load() {
		const { data, error: err } = await api.GET('/api/v1/credentials', {
			params: { query: { limit: 500, search: search || undefined, kind: kindFilter || undefined } }
		});
		loading = false;
		if (data) {
			credentials = data.items;
			total = data.total;
		} else {
			error = errorMessage(err);
		}
	}

	$effect(() => {
		void search;
		void kindFilter;
		load();
	});

	$effect(() => {
		listCredentialKinds().then((k) => (kinds = k));
		loadMyGrants();
	});

	function kindLabel(k: CredentialKind): string {
		return kinds.find((i) => i.kind === k)?.label ?? k;
	}

	/** The credential's public values, e.g. its username and server. */
	function summary(c: Credential): string {
		return Object.values(c.publicInputs).filter(Boolean).join(' · ');
	}

	async function create(event: SubmitEvent) {
		event.preventDefault();
		if (!inputsEl) return;
		saving = true;
		formError = '';
		const { data, error: err } = await api.POST('/api/v1/credentials', {
			body: { name, description, kind, inputs: inputsEl.inputs() }
		});
		saving = false;
		if (data) goto(`/credentials/${data.id}`);
		else formError = errorMessage(err);
	}
</script>

<h1>Credentials</h1>
<p class="muted">
	Secrets jobs use: SSH keys, passwords, tokens, registry and source control logins. Secret values
	are encrypted and can't be read back; only job runners receive them.
</p>

<div class="filters">
	<label>
		Search
		<input bind:value={search} placeholder="Name contains…" />
	</label>
	<label>
		Kind
		<select bind:value={kindFilter}>
			<option value="">All kinds</option>
			{#each kinds as k (k.kind)}
				<option value={k.kind}>{k.label}</option>
			{/each}
		</select>
	</label>
</div>

{#if error}
	<p class="error" role="alert">{error}</p>
{/if}

<section class="card">
	{#if loading}
		<p class="muted">Loading…</p>
	{:else if credentials.length === 0}
		<p class="muted">No credentials{search || kindFilter ? ' match' : ' yet'}.</p>
	{:else}
		<div class="table-wrap">
			<table>
				<thead>
					<tr>
						<th>Name</th>
						<th>Kind</th>
						<th>Details</th>
						<th>Updated</th>
					</tr>
				</thead>
				<tbody>
					{#each credentials as c (c.id)}
						<tr>
							<td>
								<a href="/credentials/{c.id}">{c.name}</a>
								{#if c.description}<div class="muted small">{c.description}</div>{/if}
							</td>
							<td>{kindLabel(c.kind)}</td>
							<td class="small"><code>{summary(c)}</code></td>
							<td class="small">{fmtTime(c.updatedAt)}</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
		{#if total > credentials.length}
			<p class="muted">Showing {credentials.length} of {total}. Narrow the search to see others.</p>
		{/if}
	{/if}
</section>

{#if mayCreate}
	<section class="card">
		<h2>New credential</h2>
		{#if formError}<p class="error" role="alert">{formError}</p>{/if}
		<form onsubmit={create}>
			<div class="form-grid">
				<label>Name <input bind:value={name} maxlength="255" required /></label>
				<label>Description <input bind:value={description} maxlength="4096" /></label>
				<label>
					Kind
					<select bind:value={kind}>
						{#each kinds as k (k.kind)}
							<option value={k.kind}>{k.label}</option>
						{/each}
					</select>
				</label>
			</div>
			{#if kindInfo}
				{#key kindInfo.kind}
					<CredentialInputs bind:this={inputsEl} kind={kindInfo} />
				{/key}
			{/if}
			<div class="form-actions">
				<button type="submit" disabled={saving || !kindInfo}>
					{saving ? 'Creating…' : 'Create credential'}
				</button>
			</div>
		</form>
	</section>
{/if}

<style>
	.filters {
		display: flex;
		flex-wrap: wrap;
		gap: 0 1rem;
	}

	.filters label {
		width: 280px;
	}
</style>
