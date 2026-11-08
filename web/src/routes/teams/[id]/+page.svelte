<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import {
		api,
		errorMessage,
		type RoleGrant,
		type Team,
		type TeamMember,
		type User
	} from '#lib/api/client.js';
	import GrantForm from '#lib/components/GrantForm.svelte';
	import GrantTable from '#lib/components/GrantTable.svelte';
	import { fmtTime } from '#lib/format.js';
	import { session } from '#lib/session.svelte.js';

	const id = $derived(page.params.id ?? '');
	const isAdmin = $derived(Boolean(session.user?.isAdmin));
	const canSeeGrants = $derived(Boolean(session.user?.isAdmin || session.user?.isAuditor));

	let team = $state<Team | null>(null);
	let members = $state<TeamMember[]>([]);
	let grants = $state<RoleGrant[]>([]);
	let users = $state<User[]>([]);
	let error = $state('');

	let name = $state('');
	let description = $state('');
	let oidcGroups = $state('');
	let newMember = $state('');

	const nonMembers = $derived(users.filter((u) => !members.some((m) => m.user.id === u.id)));

	async function load() {
		const path = { params: { path: { teamId: id } } };
		const { data, error: err } = await api.GET('/api/v1/teams/{teamId}', path);
		if (!data) {
			error = errorMessage(err);
			return;
		}
		team = data;
		name = data.name;
		description = data.description;
		oidcGroups = data.oidcGroups.join(', ');
		members = (await api.GET('/api/v1/teams/{teamId}/members', path)).data?.items ?? [];
		if (canSeeGrants) {
			const g = await api.GET('/api/v1/role-grants', {
				params: { query: { teamId: id, limit: 500 } }
			});
			grants = g.data?.items ?? [];
		}
		if (isAdmin) {
			const u = await api.GET('/api/v1/users', { params: { query: { limit: 500 } } });
			users = u.data?.items ?? [];
		}
	}

	$effect(() => {
		void id;
		load();
	});

	async function save(event: SubmitEvent) {
		event.preventDefault();
		error = '';
		const { data, error: err } = await api.PATCH('/api/v1/teams/{teamId}', {
			params: { path: { teamId: id } },
			body: {
				name,
				description,
				oidcGroups: oidcGroups
					.split(',')
					.map((x) => x.trim())
					.filter(Boolean)
			}
		});
		if (data) team = data;
		else error = errorMessage(err);
	}

	async function addMember(event: SubmitEvent) {
		event.preventDefault();
		const { error: err } = await api.PUT('/api/v1/teams/{teamId}/members/{userId}', {
			params: { path: { teamId: id, userId: newMember } }
		});
		if (err) error = errorMessage(err);
		newMember = '';
		await load();
	}

	async function removeMember(m: TeamMember) {
		const { error: err } = await api.DELETE('/api/v1/teams/{teamId}/members/{userId}', {
			params: { path: { teamId: id, userId: m.user.id } }
		});
		if (err) error = errorMessage(err);
		await load();
	}

	async function revoke(g: RoleGrant) {
		const { error: err } = await api.DELETE('/api/v1/role-grants/{grantId}', {
			params: { path: { grantId: g.id } }
		});
		if (err) error = errorMessage(err);
		await load();
	}

	async function remove() {
		if (!team || !confirm(`Delete team ${team.name}? Its roles are revoked.`)) return;
		const { error: err } = await api.DELETE('/api/v1/teams/{teamId}', {
			params: { path: { teamId: id } }
		});
		if (err) error = errorMessage(err);
		else goto('/teams');
	}
</script>

<p><a href="/teams">← Teams</a></p>

{#if error}
	<p class="error" role="alert">{error}</p>
{/if}

{#if team}
	<h1>{team.name}</h1>
	{#if team.description}<p class="muted">{team.description}</p>{/if}

	{#if isAdmin}
		<section class="card">
			<h2>Settings</h2>
			<form onsubmit={save} class="form-grid">
				<label>Name <input bind:value={name} maxlength="255" required /></label>
				<label>Description <input bind:value={description} /></label>
				<label>
					OIDC groups
					<input bind:value={oidcGroups} placeholder="Comma-separated" />
				</label>
				<label><button type="submit">Save</button></label>
			</form>
			<p class="muted small">
				Group changes apply to each member the next time they sign in.
				<button class="danger" onclick={remove}>Delete team</button>
			</p>
		</section>
	{/if}

	<section class="card">
		<h2>Members</h2>
		{#if members.length === 0}
			<p class="muted">No members yet.</p>
		{:else}
			<div class="table-wrap">
				<table>
					<thead>
						<tr>
							<th>User</th>
							<th>Joined through</th>
							<th>Since</th>
							{#if isAdmin}<th></th>{/if}
						</tr>
					</thead>
					<tbody>
						{#each members as m (m.user.id)}
							<tr>
								<td>
									<a href="/users/{m.user.id}">{m.user.username}</a>
									{#if m.displayName}<span class="muted">({m.displayName})</span>{/if}
								</td>
								<td>{m.source === 'oidc' ? 'OIDC group' : 'Added by an administrator'}</td>
								<td>{fmtTime(m.addedAt)}</td>
								{#if isAdmin}
									<td class="row-actions">
										<button class="danger" onclick={() => removeMember(m)}>Remove</button>
									</td>
								{/if}
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
		{#if isAdmin}
			<form onsubmit={addMember} class="form-grid add">
				<label>
					Add a user
					<select bind:value={newMember} required>
						<option value="" disabled>Choose…</option>
						{#each nonMembers as u (u.id)}
							<option value={u.id}>{u.username}{u.displayName ? ` (${u.displayName})` : ''}</option>
						{/each}
					</select>
				</label>
				<label><button type="submit">Add</button></label>
			</form>
			<p class="muted small">
				Members added here stay members whatever their OIDC groups. A member who joined through an
				OIDC group and is removed here joins again at their next sign-in, unless the team's groups
				change.
			</p>
		{/if}
	</section>

	{#if canSeeGrants}
		<section class="card">
			<h2>Roles</h2>
			{#if grants.length === 0}
				<p class="muted">The team holds no roles.</p>
			{:else}
				<GrantTable {grants} showHolder={false} onrevoke={isAdmin ? revoke : undefined} />
			{/if}
			{#if isAdmin}
				<h3>Grant a role</h3>
				<GrantForm teamId={id} ongranted={load} />
			{/if}
		</section>
	{/if}
{/if}

<style>
	h3 {
		font-size: 1rem;
		margin: 1.25rem 0 0.5rem;
	}

	.add {
		margin-top: 1rem;
	}
</style>
