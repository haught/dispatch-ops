<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, errorMessage } from '#lib/api/client.js';
	import { session } from '#lib/session.svelte.js';

	let username = $state('');
	let password = $state('');
	let error = $state('');
	let busy = $state(false);

	// Only follow same-site paths, so the login page can't be used as an open redirect.
	function nextPath(): string {
		const next = page.url.searchParams.get('next') ?? '/';
		return next.startsWith('/') && !next.startsWith('//') ? next : '/';
	}

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		busy = true;
		error = '';
		const { data, error: err } = await api.POST('/api/v1/auth/login', {
			body: { username, password }
		});
		busy = false;
		if (!data) {
			error = errorMessage(err);
			return;
		}
		session.user = data;
		session.loaded = true;
		goto(nextPath(), { replaceState: true });
	}
</script>

<div class="wrap">
	<form class="card" onsubmit={submit}>
		<h1>Dispatch Ops</h1>
		<label>
			Username
			<input bind:value={username} autocomplete="username" required />
		</label>
		<label>
			Password
			<input type="password" bind:value={password} autocomplete="current-password" required />
		</label>
		{#if error}
			<p class="error" role="alert">{error}</p>
		{/if}
		<button type="submit" disabled={busy}>{busy ? 'Signing in…' : 'Sign in'}</button>
		<p class="muted hint">Single sign-on (OIDC) arrives in a later milestone.</p>
	</form>
</div>

<style>
	.wrap {
		min-height: 100vh;
		display: grid;
		place-items: center;
		padding: 1rem;
	}

	form {
		width: 100%;
		max-width: 360px;
	}

	button {
		width: 100%;
	}

	.hint {
		font-size: 0.85rem;
		margin: 1rem 0 0;
	}
</style>
