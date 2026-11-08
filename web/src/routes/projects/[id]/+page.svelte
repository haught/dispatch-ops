<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import {
		api,
		errorMessage,
		listTemplates,
		type Credential,
		type JobTemplate,
		type Project
	} from '#lib/api/client.js';
	import ResourceAccess from '#lib/components/ResourceAccess.svelte';
	import UsedBy from '#lib/components/UsedBy.svelte';
	import { fmtTime } from '#lib/format.js';
	import { can, loadMyGrants } from '#lib/permissions.svelte.js';

	const id = $derived(page.params.id ?? '');

	let project = $state<Project | null>(null);
	let credentials = $state<Credential[]>([]);
	let templates = $state<JobTemplate[]>([]);
	let error = $state('');

	let name = $state('');
	let description = $state('');
	let scmUrl = $state('');
	let scmRef = $state('');
	let credentialId = $state('');
	let saving = $state(false);
	let message = $state('');
	let formError = $state('');

	const canAdmin = $derived(can('project', id, 'admin'));
	const credential = $derived(credentials.find((c) => c.id === project?.credentialId));

	function fill(p: Project) {
		project = p;
		name = p.name;
		description = p.description;
		scmUrl = p.scmUrl;
		scmRef = p.scmRef;
		credentialId = p.credentialId ?? '';
	}

	$effect(() => {
		const pid = id;
		if (!pid) return;
		project = null;
		error = '';
		api
			.GET('/api/v1/projects/{projectId}', { params: { path: { projectId: pid } } })
			.then(({ data, error: err }) => {
				if (data) fill(data);
				else error = errorMessage(err);
			});
		listTemplates().then((t) => (templates = t.filter((x) => x.projectId === pid)));
	});

	$effect(() => {
		loadMyGrants();
		api
			.GET('/api/v1/credentials', { params: { query: { kind: 'scm', limit: 500 } } })
			.then(({ data }) => (credentials = data?.items ?? []));
	});

	async function save(event: SubmitEvent) {
		event.preventDefault();
		if (!project) return;
		saving = true;
		formError = message = '';
		const { data, error: err } = await api.PATCH('/api/v1/projects/{projectId}', {
			params: { path: { projectId: project.id } },
			body: {
				name,
				description,
				scmUrl: scmUrl.trim(),
				scmRef: scmRef.trim(),
				// Only send the credential when it changed: keeping one you can't use is allowed.
				...(credentialId !== (project.credentialId ?? '')
					? { credentialId: credentialId || null }
					: {})
			}
		});
		saving = false;
		if (!data) {
			formError = errorMessage(err);
			return;
		}
		fill(data);
		message = 'Saved. Jobs already launched keep the repository and ref they started with.';
	}

	async function remove() {
		if (!project || !confirm(`Delete the project ${project.name}?`)) return;
		error = '';
		const { error: err } = await api.DELETE('/api/v1/projects/{projectId}', {
			params: { path: { projectId: project.id } }
		});
		if (err) error = errorMessage(err);
		else goto('/projects');
	}
</script>

{#if project}
	<div class="page-header">
		<h1>{project.name}</h1>
		<span class="spacer"></span>
		{#if canAdmin}<button class="danger" onclick={remove}>Delete</button>{/if}
	</div>
	{#if project.description}<p class="muted">{project.description}</p>{/if}
	{#if error}<p class="error" role="alert">{error}</p>{/if}

	{#if canAdmin}
		<section class="card">
			{#if formError}<p class="error" role="alert">{formError}</p>{/if}
			<form onsubmit={save}>
				<div class="form-grid">
					<label>Name <input bind:value={name} maxlength="255" required /></label>
					<label>Description <input bind:value={description} maxlength="4096" /></label>
				</div>
				<div class="form-grid">
					<label class="wide">
						Repository URL <input bind:value={scmUrl} required maxlength="2048" />
					</label>
					<label>
						Branch, tag or commit <input bind:value={scmRef} required maxlength="255" />
					</label>
					<label>
						Credential
						<select bind:value={credentialId}>
							<option value="">None (public repository)</option>
							{#if project.credentialId && !credential}
								<option value={project.credentialId}>(a credential you can't see)</option>
							{/if}
							{#each credentials as c (c.id)}
								<option value={c.id}>{c.name}</option>
							{/each}
						</select>
					</label>
				</div>
				<div class="form-actions">
					<button type="submit" disabled={saving}>{saving ? 'Saving…' : 'Save'}</button>
					{#if message}<span class="muted small">{message}</span>{/if}
				</div>
			</form>
			<p class="muted small">
				Created {fmtTime(project.createdAt)} · updated {fmtTime(project.updatedAt)}
			</p>
		</section>
	{:else}
		<section class="card">
			<dl class="details">
				<dt>Repository</dt>
				<dd><code>{project.scmUrl}</code></dd>
				<dt>Ref</dt>
				<dd><code>{project.scmRef}</code></dd>
				<dt>Credential</dt>
				<dd>
					{#if credential}<a href="/credentials/{credential.id}">{credential.name}</a>
					{:else if project.credentialId}a credential you can't see
					{:else}none{/if}
				</dd>
				<dt>Updated</dt>
				<dd>{fmtTime(project.updatedAt)}</dd>
			</dl>
		</section>
	{/if}

	<UsedBy {templates} what="project" />
	<ResourceAccess type="project" id={project.id} />
{:else if error}
	<p class="error" role="alert">{error}</p>
	<p><a href="/projects">Back to projects</a></p>
{:else}
	<p class="muted">Loading…</p>
{/if}

<style>
	.wide {
		grid-column: 1 / -1;
	}
</style>
