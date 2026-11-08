<script lang="ts">
	import { goto } from '$app/navigation';
	import { api, errorMessage, type Credential, type Project } from '#lib/api/client.js';
	import { canCreate, loadMyGrants } from '#lib/permissions.svelte.js';

	let projects = $state<Project[]>([]);
	let total = $state(0);
	let credentials = $state<Credential[]>([]);
	let search = $state('');
	let loading = $state(true);
	let error = $state('');

	let name = $state('');
	let description = $state('');
	let scmUrl = $state('');
	let scmRef = $state('main');
	let credentialId = $state('');
	let formError = $state('');
	let saving = $state(false);

	const mayCreate = $derived(canCreate('project'));

	async function load() {
		const { data, error: err } = await api.GET('/api/v1/projects', {
			params: { query: { limit: 500, search: search || undefined } }
		});
		loading = false;
		if (data) {
			projects = data.items;
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
		loadMyGrants();
		api
			.GET('/api/v1/credentials', { params: { query: { kind: 'scm', limit: 500 } } })
			.then(({ data }) => (credentials = data?.items ?? []));
	});

	function credentialName(id: string | null): string {
		if (!id) return '';
		return credentials.find((c) => c.id === id)?.name ?? 'a credential';
	}

	async function create(event: SubmitEvent) {
		event.preventDefault();
		saving = true;
		formError = '';
		const { data, error: err } = await api.POST('/api/v1/projects', {
			body: {
				name,
				description,
				scmUrl: scmUrl.trim(),
				scmRef: scmRef.trim() || 'main',
				credentialId: credentialId || undefined
			}
		});
		saving = false;
		if (data) goto(`/projects/${data.id}`);
		else formError = errorMessage(err);
	}
</script>

<h1>Projects</h1>
<p class="muted">
	Git repositories holding playbooks and scripts. Ansible and script jobs clone their project when
	they start.
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
	{:else if projects.length === 0}
		<p class="muted">No projects{search ? ' match' : ' yet'}.</p>
	{:else}
		<div class="table-wrap">
			<table>
				<thead>
					<tr>
						<th>Name</th>
						<th>Repository</th>
						<th>Ref</th>
						<th>Credential</th>
					</tr>
				</thead>
				<tbody>
					{#each projects as p (p.id)}
						<tr>
							<td>
								<a href="/projects/{p.id}">{p.name}</a>
								{#if p.description}<div class="muted small">{p.description}</div>{/if}
							</td>
							<td class="small"><code>{p.scmUrl}</code></td>
							<td><code>{p.scmRef}</code></td>
							<td class="small">
								{#if p.credentialId}
									<a href="/credentials/{p.credentialId}">{credentialName(p.credentialId)}</a>
								{/if}
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
		{#if total > projects.length}
			<p class="muted">Showing {projects.length} of {total}. Narrow the search to see others.</p>
		{/if}
	{/if}
</section>

{#if mayCreate}
	<section class="card">
		<h2>New project</h2>
		{#if formError}<p class="error" role="alert">{formError}</p>{/if}
		<form onsubmit={create}>
			<div class="form-grid">
				<label>Name <input bind:value={name} maxlength="255" required /></label>
				<label>Description <input bind:value={description} maxlength="4096" /></label>
			</div>
			<div class="form-grid">
				<label class="wide">
					Repository URL
					<input
						bind:value={scmUrl}
						required
						maxlength="2048"
						placeholder="https://github.com/acme/playbooks.git or git@host:acme/playbooks.git"
					/>
				</label>
				<label>
					Branch, tag or commit
					<input bind:value={scmRef} maxlength="255" placeholder="main" />
				</label>
				<label>
					Credential
					<select bind:value={credentialId}>
						<option value="">None (public repository)</option>
						{#each credentials as c (c.id)}
							<option value={c.id}>{c.name}</option>
						{/each}
					</select>
				</label>
			</div>
			<p class="muted small">
				Private repositories need a <a href="/credentials">source control credential</a> you can use.
			</p>
			<div class="form-actions">
				<button type="submit" disabled={saving}>{saving ? 'Creating…' : 'Create project'}</button>
			</div>
		</form>
	</section>
{/if}

<style>
	.search {
		max-width: 320px;
	}

	.wide {
		grid-column: 1 / -1;
	}
</style>
