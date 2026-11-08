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
	let creating = $state(false);

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

	async function create(event: SubmitEvent) {
		event.preventDefault();
		creating = true;
		error = '';
		const { error: err } = await api.POST('/api/v1/users', {
			body: { username, displayName, email, password }
		});
		creating = false;
		if (err) {
			error = errorMessage(err);
			return;
		}
		username = displayName = email = password = '';
		await load();
	}
</script>

<h1>Users</h1>
<p class="muted">
	Single sign-on users appear when they first sign in. Local users sign in with a password and are
	meant for automation (service accounts).
</p>

{#if error}
	<p class="error" role="alert">{error}</p>
{/if}

{#if isAdmin}
	<section class="card">
		<h2>New local user</h2>
		<form onsubmit={create} class="form-grid">
			<label>
				Username
				<input bind:value={username} required autocomplete="off" />
			</label>
			<label>
				Display name
				<input bind:value={displayName} />
			</label>
			<label>
				Email
				<input type="email" bind:value={email} />
			</label>
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
			<label><button type="submit" disabled={creating}>Create user</button></label>
		</form>
	</section>
{/if}

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

<style>
	.search {
		max-width: 320px;
	}

	tr.inactive td {
		color: var(--muted);
	}
</style>
