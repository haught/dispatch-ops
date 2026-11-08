<script lang="ts">
	import '../app.css';
	import favicon from '#lib/assets/favicon.svg';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { onUnauthorized } from '#lib/api/client.js';
	import { loadSession, session, signOut } from '#lib/session.svelte.js';
	import type { LayoutProps } from './$types';

	let { children }: LayoutProps = $props();

	const nav = [
		{ href: '/', label: 'Dashboard' },
		{ href: '/tokens', label: 'API tokens' }
	];
	// Sections from PLAN.md that arrive in later milestones.
	const upcoming = [
		'Job templates',
		'Jobs',
		'Inventories',
		'Credentials',
		'Projects',
		'Execution environments'
	];

	const isLogin = $derived(page.url.pathname === '/login');

	loadSession();

	onUnauthorized(() => {
		session.user = null;
	});

	// Send signed-out visitors to the login page, remembering where they were going.
	$effect(() => {
		if (session.loaded && !session.user && !isLogin) {
			const next = page.url.pathname + page.url.search;
			goto(`/login?next=${encodeURIComponent(next)}`, { replaceState: true });
		}
	});

	async function handleSignOut() {
		await signOut();
		goto('/login');
	}

	function isActive(href: string) {
		return href === '/' ? page.url.pathname === '/' : page.url.pathname.startsWith(href);
	}
</script>

<svelte:head>
	<link rel="icon" href={favicon} />
	<title>Dispatch Ops</title>
</svelte:head>

{#if isLogin}
	{@render children()}
{:else if session.user}
	<div class="shell">
		<aside>
			<div class="brand">Dispatch Ops</div>
			<nav>
				{#each nav as item (item.href)}
					<a href={item.href} class:active={isActive(item.href)}>{item.label}</a>
				{/each}
				<div class="section">Coming soon</div>
				{#each upcoming as label (label)}
					<span class="disabled">{label}</span>
				{/each}
			</nav>
		</aside>
		<div class="main">
			<header>
				<span class="muted">Signed in as <strong>{session.user.username}</strong></span>
				<button class="secondary" onclick={handleSignOut}>Sign out</button>
			</header>
			<main>
				{@render children()}
			</main>
		</div>
	</div>
{/if}

<style>
	.shell {
		display: grid;
		grid-template-columns: 220px 1fr;
		min-height: 100vh;
	}

	aside {
		background: var(--surface);
		border-right: 1px solid var(--border);
		padding: 1rem 0.75rem;
	}

	.brand {
		font-weight: 700;
		font-size: 1.1rem;
		padding: 0 0.5rem 1rem;
	}

	nav {
		display: flex;
		flex-direction: column;
		gap: 2px;
	}

	nav a,
	nav .disabled {
		padding: 0.4rem 0.5rem;
		border-radius: 6px;
		color: var(--text);
		text-decoration: none;
	}

	nav a:hover {
		background: var(--bg);
	}

	nav a.active {
		background: var(--bg);
		font-weight: 600;
	}

	nav .disabled {
		color: var(--muted);
	}

	nav .section {
		margin: 1rem 0.5rem 0.25rem;
		font-size: 0.75rem;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--muted);
	}

	.main {
		display: flex;
		flex-direction: column;
		min-width: 0;
	}

	header {
		display: flex;
		justify-content: flex-end;
		align-items: center;
		gap: 1rem;
		padding: 0.75rem 1.5rem;
		border-bottom: 1px solid var(--border);
		background: var(--surface);
	}

	main {
		padding: 1.5rem;
		max-width: 1100px;
		width: 100%;
	}

	@media (max-width: 700px) {
		.shell {
			grid-template-columns: 1fr;
		}

		aside {
			border-right: none;
			border-bottom: 1px solid var(--border);
		}

		main {
			padding: 1rem;
		}
	}
</style>
