<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, errorMessage } from '#lib/api/client.js';
	import { session } from '#lib/session.svelte.js';

	let username = $state('');
	let password = $state('');
	// Single sign-on reports failures by sending the browser here with ?error=.
	let error = $state(page.url.searchParams.get('error') ?? '');
	let busy = $state(false);
	let sso = $state<{ name: string } | null>(null);

	$effect(() => {
		api.GET('/api/v1/auth/config').then(({ data }) => {
			if (data?.oidcEnabled) sso = { name: data.oidcDisplayName };
		});
	});

	// Only follow paths on this origin, so the login page can't be used as an
	// open redirect. Browsers drop tabs and newlines from URLs and treat "\"
	// like "/", so "/\t/evil.com" would lead off-site: reject control
	// characters and backslashes, before and after decoding. This matches
	// safeNext in internal/api/oidc.go.
	function isLocalPath(p: string): boolean {
		let decoded: string;
		try {
			decoded = decodeURIComponent(p);
		} catch {
			return false;
		}
		for (const s of [p, decoded]) {
			if (!s.startsWith('/') || s.startsWith('//') || s.includes('\\')) return false;
			// eslint-disable-next-line no-control-regex
			if (/[\x00-\x1f\x7f]/.test(s)) return false;
		}
		try {
			const base = new URL('https://origin.invalid/');
			const u = new URL(p, base);
			return u.origin === base.origin && !u.username && !u.password;
		} catch {
			return false;
		}
	}

	function nextPath(): string {
		const next = page.url.searchParams.get('next') ?? '/';
		return isLocalPath(next) ? next : '/';
	}

	const ssoHref = $derived(`/api/v1/auth/oidc/login?next=${encodeURIComponent(nextPath())}`);

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
	<div class="card">
		<h1>Dispatch Ops</h1>
		{#if error}
			<p class="error" role="alert">{error}</p>
		{/if}
		{#if sso}
			<a class="sso" href={ssoHref}>Sign in with {sso.name}</a>
			<div class="or muted"><span>or with a local account</span></div>
		{/if}
		<form onsubmit={submit}>
			<label>
				Username
				<input bind:value={username} autocomplete="username" required />
			</label>
			<label>
				Password
				<input type="password" bind:value={password} autocomplete="current-password" required />
			</label>
			<button type="submit" class:secondary={sso} disabled={busy}
				>{busy ? 'Signing in…' : 'Sign in'}</button
			>
		</form>
	</div>
</div>

<style>
	.wrap {
		min-height: 100vh;
		display: grid;
		place-items: center;
		padding: 1rem;
	}

	.card {
		width: 100%;
		max-width: 360px;
	}

	button {
		width: 100%;
	}

	.sso {
		display: block;
		text-align: center;
		padding: 0.5rem 0.9rem;
		border-radius: 6px;
		background: var(--accent);
		color: var(--accent-text);
		text-decoration: none;
	}

	.or {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		margin: 1rem 0;
		font-size: 0.85rem;
	}

	.or::before,
	.or::after {
		content: '';
		flex: 1;
		border-top: 1px solid var(--border);
	}
</style>
