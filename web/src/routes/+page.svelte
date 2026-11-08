<script lang="ts">
	import { api, type Health } from '#lib/api/client.js';
	import { session } from '#lib/session.svelte.js';

	let health = $state<Health | null>(null);

	$effect(() => {
		api.GET('/api/v1/health').then(({ data, error }) => {
			health = data ?? (error as Health | undefined) ?? null;
		});
	});
</script>

<h1>Welcome, {session.user?.displayName || session.user?.username}</h1>

<div class="grid">
	<section class="card">
		<h2>System</h2>
		{#if health}
			<dl>
				<dt>Status</dt>
				<dd class:ok={health.status === 'ok'} class:error={health.status !== 'ok'}>
					{health.status}
				</dd>
				<dt>Database</dt>
				<dd class:ok={health.database === 'ok'} class:error={health.database !== 'ok'}>
					{health.database}
				</dd>
				<dt>Version</dt>
				<dd>{health.version ?? 'unknown'}</dd>
			</dl>
		{:else}
			<p class="muted">Checking…</p>
		{/if}
	</section>

	<section class="card">
		<h2>Getting started</h2>
		<p>
			This is the milestone 1 skeleton. Create an <a href="/tokens">API token</a> to call the API
			from scripts, or browse the spec at <code>api/openapi.yaml</code>.
		</p>
		<p class="muted">
			Job templates, inventories, credentials and execution environments come next.
		</p>
	</section>
</div>

<style>
	.grid {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
		gap: 1rem;
	}

	dl {
		display: grid;
		grid-template-columns: max-content 1fr;
		gap: 0.25rem 1rem;
		margin: 0;
	}

	dt {
		color: var(--muted);
	}

	dd {
		margin: 0;
	}

	.ok {
		color: var(--ok);
	}

	p:last-child {
		margin-bottom: 0;
	}
</style>
