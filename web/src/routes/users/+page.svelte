<script lang="ts">
	import { api, errorMessage, type User } from '#lib/api/client.js';
	import { fmtTime } from '#lib/format.js';
	import { session } from '#lib/session.svelte.js';

	let users = $state<User[]>([]);
	let total = $state(0);
	let search = $state('');
	let loading = $state(true);
	let error = $state('');

	let username = $state('');
	let displayName = $state('');
	let email = $state('');
	let password = $state('');
	let authSource = $state<'local' | 'oidc'>('local');
	let creating = $state(false);
	// The name of the sign-on provider, if single sign-on is configured: only
	// then can single sign-on users be added, and the choice is disabled
	// otherwise.
	let sso = $state<string | null>(null);
	// Adding shows the form alone, in place of the list.
	let adding = $state(false);

	const isAdmin = $derived(Boolean(session.user?.isAdmin));

	async function load() {
		const { data, error: err } = await api.GET('/api/v1/users', {
			params: { query: { limit: 500, search: search || undefined } }
		});
		loading = false;
		if (data) {
			users = data.items;
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
		api.GET('/api/v1/auth/config').then(({ data }) => {
			if (data?.oidcEnabled) sso = data.oidcDisplayName;
		});
	});

	async function create(event: SubmitEvent) {
		event.preventDefault();
		creating = true;
		error = '';
		const { error: err } = await api.POST('/api/v1/users', {
			body:
				authSource === 'oidc'
					? { authSource, username, displayName, email }
					: { authSource, username, displayName, email, password }
		});
		creating = false;
		if (err) {
			error = errorMessage(err);
			return;
		}
		username = displayName = email = password = '';
		authSource = 'local';
		adding = false;
		await load();
	}
</script>

<div class="page-header">
	<h1>Users</h1>
	<span class="spacer"></span>
	{#if isAdmin && !adding}
		<button onclick={() => (adding = true)}>New user</button>
	{/if}
</div>
<p class="muted">
	Single sign-on users appear when they first sign in, or can be added beforehand to give them roles
	and teams in advance. Local users sign in with a password and are meant for automation (service
	accounts).
</p>

{#if error}
	<p class="error" role="alert">{error}</p>
{/if}

{#if isAdmin && adding}
	<section class="card">
		<div class="page-header">
			<h2>New user</h2>
			<span class="spacer"></span>
			<button type="button" class="secondary" onclick={() => (adding = false)}>Cancel</button>
		</div>
		<form onsubmit={create}>
			<div class="field">
				Sign-in
				<div class="types" role="radiogroup">
					<label class:chosen={authSource === 'local'}>
						<input type="radio" name="authSource" bind:group={authSource} value="local" />
						<span>
							<strong>Local</strong>
							<span class="muted small">Signs in with a password you set here.</span>
						</span>
					</label>
					<label class:chosen={authSource === 'oidc'}>
						<input
							type="radio"
							name="authSource"
							bind:group={authSource}
							value="oidc"
							disabled={!sso}
						/>
						<span>
							<strong>{sso ?? 'Single sign-on'}</strong>
							<span class="muted small">
								{#if sso}
									Signs in through the provider. The username must be the one the provider sends:
									whoever first signs in with it becomes this user.
								{:else}
									Not available: single sign-on isn't configured on this server.
								{/if}
							</span>
						</span>
					</label>
				</div>
			</div>
			<div class="form-grid">
				<label>
					Username
					<input bind:value={username} required pattern="\S+" autocomplete="off" />
				</label>
				<label>
					Display name
					<input bind:value={displayName} />
				</label>
				<label>
					Email
					<input type="email" bind:value={email} />
				</label>
				{#if authSource === 'local'}
					<label>
						Password
						<input
							type="password"
							bind:value={password}
							minlength="12"
							required
							autocomplete="new-password"
						/>
					</label>
				{/if}
				<label><button type="submit" disabled={creating}>Create user</button></label>
			</div>
		</form>
	</section>
{/if}

{#if !adding}
	<section class="card">
		<label class="search">
			Search
			<input bind:value={search} placeholder="Username, name or email…" />
		</label>
		{#if loading}
			<p class="muted">Loading…</p>
		{:else if users.length === 0}
			<p class="muted">No users match.</p>
		{:else}
			<div class="table-wrap">
				<table>
					<thead>
						<tr>
							<th>Username</th>
							<th>Name</th>
							<th>Sign-in</th>
							<th>Global role</th>
							<th>Last sign-in</th>
						</tr>
					</thead>
					<tbody>
						{#each users as u (u.id)}
							<tr class:inactive={!u.isActive}>
								<td>
									<a href="/users/{u.id}">{u.username}</a>
									{#if !u.isActive}<span class="badge warn">deactivated</span>{/if}
									{#if u.oidcPending}<span class="badge">awaiting first sign-in</span>{/if}
								</td>
								<td>{u.displayName}</td>
								<td>{u.authSource === 'oidc' ? 'Single sign-on' : 'Local'}</td>
								<td>
									{#if u.isAdmin}<span class="badge accent">system_admin</span>{/if}
									{#if u.isAuditor}<span class="badge">auditor</span>{/if}
								</td>
								<td>{fmtTime(u.lastLoginAt)}</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
			{#if total > users.length}
				<p class="muted">Showing {users.length} of {total}. Narrow the search to see others.</p>
			{/if}
		{/if}
	</section>
{/if}

<style>
	.search {
		max-width: 320px;
	}

	tr.inactive td {
		color: var(--muted);
	}

	.field {
		font-weight: 500;
		margin-bottom: 0.75rem;
	}

	.types {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
		gap: 0.5rem;
		margin-top: 0.25rem;
	}

	.types label {
		display: flex;
		gap: 0.5rem;
		align-items: flex-start;
		margin: 0;
		padding: 0.6rem 0.75rem;
		border: 1px solid var(--border);
		border-radius: 6px;
		font-weight: normal;
		cursor: pointer;
	}

	.types label.chosen {
		border-color: var(--accent);
	}

	.types label:has(input:disabled) {
		cursor: default;
		opacity: 0.5;
	}

	.types input {
		margin-top: 0.2rem;
	}

	.types span {
		display: flex;
		flex-direction: column;
	}
</style>
