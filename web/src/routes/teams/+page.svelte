<script lang="ts">
	import { api, errorMessage, type Team } from '#lib/api/client.js';
	import { session } from '#lib/session.svelte.js';

	let teams = $state<Team[]>([]);
	let loading = $state(true);
	let error = $state('');

	let name = $state('');
	let description = $state('');
	let oidcGroups = $state('');

	const isAdmin = $derived(Boolean(session.user?.isAdmin));

	async function load() {
		const { data, error: err } = await api.GET('/api/v1/teams', {
			params: { query: { limit: 500 } }
		});
		loading = false;
		if (data) teams = data.items;
		else error = errorMessage(err);
	}

	$effect(() => {
		load();
	});

	async function create(event: SubmitEvent) {
		event.preventDefault();
		error = '';
		const { error: err } = await api.POST('/api/v1/teams', {
			body: { name, description, oidcGroups: splitList(oidcGroups) }
		});
		if (err) {
			error = errorMessage(err);
			return;
		}
		name = description = oidcGroups = '';
		await load();
	}

	function splitList(s: string): string[] {
		return s
			.split(',')
			.map((x) => x.trim())
			.filter(Boolean);
	}
</script>

<h1>Teams</h1>
<p class="muted">
	Roles granted to a team apply to its members. Single sign-on users join the teams that list one of
	their provider groups whenever they sign in, and leave them when they no longer have it.
</p>

{#if error}
	<p class="error" role="alert">{error}</p>
{/if}

{#if isAdmin}
	<section class="card">
		<h2>New team</h2>
		<form onsubmit={create} class="form-grid">
			<label>Name <input bind:value={name} maxlength="255" required /></label>
			<label>Description <input bind:value={description} /></label>
			<label>
				OIDC groups
				<input bind:value={oidcGroups} placeholder="Comma-separated, e.g. ops, sre" />
			</label>
			<label><button type="submit">Create team</button></label>
		</form>
	</section>
{/if}

<section class="card">
	{#if loading}
		<p class="muted">Loading…</p>
	{:else if teams.length === 0}
		<p class="muted">No teams yet.</p>
	{:else}
		<div class="table-wrap">
			<table>
				<thead>
					<tr>
						<th>Name</th>
						<th>OIDC groups</th>
						<th>Members</th>
					</tr>
				</thead>
				<tbody>
					{#each teams as t (t.id)}
						<tr>
							<td>
								<a href="/teams/{t.id}">{t.name}</a>
								{#if t.description}<div class="muted small">{t.description}</div>{/if}
							</td>
							<td>
								{#each t.oidcGroups as g (g)}<span class="badge">{g}</span>
								{/each}
							</td>
							<td>{t.memberCount}</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>
