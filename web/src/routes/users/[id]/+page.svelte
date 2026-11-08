<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import {
		api,
		errorMessage,
		type ApiToken,
		type CreatedApiToken,
		type RoleGrant,
		type Team,
		type TokenScope,
		type User
	} from '#lib/api/client.js';
	import GrantForm from '#lib/components/GrantForm.svelte';
	import GrantTable from '#lib/components/GrantTable.svelte';
	import { fmtTime } from '#lib/format.js';
	import { session } from '#lib/session.svelte.js';

	const id = $derived(page.params.id ?? '');
	const isAdmin = $derived(Boolean(session.user?.isAdmin));
	const canSeeAll = $derived(Boolean(session.user?.isAdmin || session.user?.isAuditor));
	const isSelf = $derived(session.user?.id === id);

	let user = $state<User | null>(null);
	let teams = $state<Team[]>([]);
	let grants = $state<RoleGrant[]>([]);
	let tokens = $state<ApiToken[]>([]);
	let error = $state('');

	let displayName = $state('');
	let email = $state('');
	let newPassword = $state('');

	let tokenName = $state('');
	let tokenScope = $state<TokenScope>('write');
	let tokenDays = $state<number | null>(90);
	let created = $state<CreatedApiToken | null>(null);

	const directGrants = $derived(grants.filter((g) => g.user));
	const teamGrants = $derived(grants.filter((g) => g.team));

	async function load() {
		const path = { params: { path: { userId: id } } };
		const { data, error: err } = await api.GET('/api/v1/users/{userId}', path);
		if (!data) {
			error = errorMessage(err);
			return;
		}
		user = data;
		displayName = data.displayName;
		email = data.email;
		const t = await api.GET('/api/v1/teams', { params: { query: { userId: id, limit: 500 } } });
		teams = t.data?.items ?? [];
		if (canSeeAll || isSelf) {
			const g = await api.GET('/api/v1/role-grants', {
				params: { query: { userId: id, limit: 500 } }
			});
			grants = g.data?.items ?? [];
		}
		if (canSeeAll) {
			const tk = await api.GET('/api/v1/users/{userId}/tokens', path);
			tokens = tk.data?.items ?? [];
		}
	}

	$effect(() => {
		void id;
		load();
	});

	async function update(body: {
		displayName?: string;
		email?: string;
		isActive?: boolean;
		password?: string;
	}) {
		error = '';
		const { data, error: err } = await api.PATCH('/api/v1/users/{userId}', {
			params: { path: { userId: id } },
			body
		});
		if (!data) {
			error = errorMessage(err);
			return false;
		}
		user = data;
		return true;
	}

	async function saveProfile(event: SubmitEvent) {
		event.preventDefault();
		await update({ displayName, email });
	}

	async function setPassword(event: SubmitEvent) {
		event.preventDefault();
		if (await update({ password: newPassword })) newPassword = '';
	}

	async function remove() {
		if (!user || !confirm(`Delete ${user.username}? Jobs they launched are kept.`)) return;
		const { error: err } = await api.DELETE('/api/v1/users/{userId}', {
			params: { path: { userId: id } }
		});
		if (err) error = errorMessage(err);
		else goto('/users');
	}

	async function revoke(g: RoleGrant) {
		const { error: err } = await api.DELETE('/api/v1/role-grants/{grantId}', {
			params: { path: { grantId: g.id } }
		});
		if (err) error = errorMessage(err);
		await load();
	}

	async function createToken(event: SubmitEvent) {
		event.preventDefault();
		error = '';
		const { data, error: err } = await api.POST('/api/v1/users/{userId}/tokens', {
			params: { path: { userId: id } },
			body: { name: tokenName, scope: tokenScope, expiresInDays: tokenDays || undefined }
		});
		if (!data) {
			error = errorMessage(err);
			return;
		}
		created = data;
		tokenName = '';
		await load();
	}

	async function revokeToken(t: ApiToken) {
		const { error: err } = await api.DELETE('/api/v1/users/{userId}/tokens/{tokenId}', {
			params: { path: { userId: id, tokenId: t.id } }
		});
		if (err) error = errorMessage(err);
		if (created?.id === t.id) created = null;
		await load();
	}
</script>

<p><a href="/users">← Users</a></p>

{#if error}
	<p class="error" role="alert">{error}</p>
{/if}

{#if user}
	<h1>
		{user.username}
		{#if !user.isActive}<span class="badge warn">deactivated</span>{/if}
	</h1>
	<p class="muted">
		{user.authSource === 'oidc' ? 'Single sign-on user' : 'Local user'} · created {fmtTime(
			user.createdAt
		)} · last sign-in {fmtTime(user.lastLoginAt)}
	</p>

	{#if isAdmin}
		<section class="card">
			<h2>Profile</h2>
			{#if user.authSource === 'oidc'}
				<p class="muted small">The provider updates these at each sign-in.</p>
			{/if}
			<form onsubmit={saveProfile} class="form-grid">
				<label>Display name <input bind:value={displayName} /></label>
				<label>Email <input type="email" bind:value={email} /></label>
				<label><button type="submit">Save</button></label>
			</form>
			{#if user.authSource === 'local'}
				<form onsubmit={setPassword} class="form-grid">
					<label>
						New password
						<input
							type="password"
							bind:value={newPassword}
							minlength="12"
							required
							autocomplete="new-password"
						/>
					</label>
					<label><button type="submit" class="secondary">Set password</button></label>
				</form>
			{/if}
			{#if !isSelf}
				<div class="actions">
					{#if user.isActive}
						<button class="secondary" onclick={() => update({ isActive: false })}>Deactivate</button
						>
					{:else}
						<button class="secondary" onclick={() => update({ isActive: true })}>Reactivate</button>
					{/if}
					<button class="danger" onclick={remove}>Delete user</button>
				</div>
				<p class="muted small">
					Deactivating ends the user's sessions and stops their API tokens until reactivated.
				</p>
			{/if}
		</section>
	{/if}

	<section class="card">
		<h2>Teams</h2>
		{#if teams.length === 0}
			<p class="muted">Not in any team.</p>
		{:else}
			<ul>
				{#each teams as t (t.id)}
					<li><a href="/teams/{t.id}">{t.name}</a></li>
				{/each}
			</ul>
		{/if}
	</section>

	{#if canSeeAll || isSelf}
		<section class="card">
			<h2>Roles</h2>
			{#if directGrants.length === 0}
				<p class="muted">No roles granted directly.</p>
			{:else}
				<GrantTable
					grants={directGrants}
					showHolder={false}
					onrevoke={isAdmin ? revoke : undefined}
				/>
			{/if}
			{#if teamGrants.length > 0}
				<h3>Through teams</h3>
				<GrantTable grants={teamGrants} />
			{/if}
			{#if isAdmin}
				<h3>Grant a role</h3>
				<GrantForm userId={id} ongranted={load} />
			{/if}
		</section>
	{/if}

	{#if canSeeAll}
		<section class="card">
			<h2>API tokens</h2>
			{#if created}
				<div class="created">
					<p>Token “{created.name}” created. Copy it now; it won't be shown again.</p>
					<code>{created.token}</code>
				</div>
			{/if}
			{#if tokens.length === 0}
				<p class="muted">No API tokens.</p>
			{:else}
				<div class="table-wrap">
					<table>
						<thead>
							<tr>
								<th>Name</th>
								<th>Token</th>
								<th>Scope</th>
								<th>Expires</th>
								<th>Last used</th>
								{#if isAdmin}<th></th>{/if}
							</tr>
						</thead>
						<tbody>
							{#each tokens as t (t.id)}
								<tr>
									<td>{t.name}</td>
									<td><code>{t.prefix}…</code></td>
									<td>{t.scope}</td>
									<td>{t.expiresAt ? fmtTime(t.expiresAt) : 'Never'}</td>
									<td>{fmtTime(t.lastUsedAt)}</td>
									{#if isAdmin}
										<td class="row-actions">
											<button class="danger" onclick={() => revokeToken(t)}>Revoke</button>
										</td>
									{/if}
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			{/if}
			{#if isAdmin && user.isActive}
				<h3>New token for {user.username}</h3>
				<form onsubmit={createToken} class="form-grid">
					<label>Name <input bind:value={tokenName} maxlength="100" required /></label>
					<label>
						Scope
						<select bind:value={tokenScope}>
							<option value="write">Read and write</option>
							<option value="read">Read only</option>
						</select>
					</label>
					<label>
						Expires in (days)
						<input type="number" bind:value={tokenDays} min="1" max="3650" placeholder="Never" />
					</label>
					<label><button type="submit">Create token</button></label>
				</form>
			{/if}
		</section>
	{/if}
{/if}

<style>
	h1 .badge {
		vertical-align: middle;
	}

	h3 {
		font-size: 1rem;
		margin: 1.25rem 0 0.5rem;
	}

	.actions {
		display: flex;
		gap: 0.75rem;
		margin-top: 0.5rem;
	}

	.created {
		border: 1px solid var(--ok);
		border-radius: 6px;
		padding: 0.75rem;
		margin-bottom: 1rem;
	}

	.created code {
		overflow-wrap: anywhere;
	}
</style>
