<script lang="ts">
	import { goto } from '$app/navigation';
	import { api, errorMessage, type JobTemplate } from '#lib/api/client.js';
	import LaunchDialog from '#lib/components/LaunchDialog.svelte';
	import TemplateForm from '#lib/components/TemplateForm.svelte';
	import { canCreate, loadMyGrants } from '#lib/permissions.svelte.js';

	let templates = $state<JobTemplate[]>([]);
	let total = $state(0);
	let search = $state('');
	let loading = $state(true);
	let error = $state('');
	// The template whose launch dialog is open.
	let launchTemplate = $state<JobTemplate | null>(null);
	let creating = $state(false);

	const mayCreate = $derived(canCreate('job_template'));

	$effect(() => {
		loadMyGrants();
	});

	async function load() {
		const { data, error: err } = await api.GET('/api/v1/job-templates', {
			params: { query: { limit: 500, search: search || undefined } }
		});
		loading = false;
		if (data) {
			templates = data.items;
			total = data.total;
		} else {
			error = errorMessage(err);
		}
	}

	$effect(() => {
		void search;
		load();
	});
</script>

{#if launchTemplate}
	<LaunchDialog template={launchTemplate} onclose={() => (launchTemplate = null)} />
{/if}

<div class="page-header">
	<h1>Job templates</h1>
	<span class="spacer"></span>
	{#if mayCreate && !creating}
		<button onclick={() => (creating = true)}>New job template</button>
	{/if}
</div>
<p class="muted">What to run, where and with which secrets. Launch one to start a job.</p>

{#if creating}
	<section class="card">
		<h2>New job template</h2>
		<TemplateForm onsaved={(t) => goto(`/templates/${t.id}`)} oncancel={() => (creating = false)} />
	</section>
{/if}

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
	{:else if templates.length === 0}
		<p class="muted">No job templates{search ? ' match' : ' yet'}.</p>
	{:else}
		<div class="table-wrap">
			<table>
				<thead>
					<tr>
						<th>Name</th>
						<th>Type</th>
						<th>Runs</th>
						<th></th>
					</tr>
				</thead>
				<tbody>
					{#each templates as t (t.id)}
						<tr>
							<td>
								<a href="/templates/{t.id}">{t.name}</a>
								{#if t.description}<div class="muted small">{t.description}</div>{/if}
							</td>
							<td>{t.jobType}</td>
							<td>
								<code class="command"
									>{t.jobType === 'container'
										? t.command.join(' ')
										: [t.entrypoint, ...t.arguments].join(' ')}</code
								>
							</td>
							<td>
								<button onclick={() => (launchTemplate = t)}>Launch</button>
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
		{#if total > templates.length}
			<p class="muted">Showing {templates.length} of {total}. Narrow the search to see others.</p>
		{/if}
	{/if}
</section>

<style>
	.search {
		max-width: 320px;
	}

	section.card + label.search {
		margin-top: 1rem;
	}

	.table-wrap {
		overflow-x: auto;
	}

	.small {
		font-size: 0.85rem;
	}

	.command {
		display: inline-block;
		max-width: 40ch;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		vertical-align: bottom;
	}
</style>
