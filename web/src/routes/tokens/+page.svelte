<script lang="ts">
	import { api, errorMessage, type ApiToken, type CreatedApiToken } from '#lib/api/client.js';

	let tokens = $state<ApiToken[]>([]);
	let loading = $state(true);
	let error = $state('');

	let name = $state('');
	let expiresInDays = $state<number | null>(90);
	let creating = $state(false);
	let created = $state<CreatedApiToken | null>(null);
	let copied = $state(false);

	async function load() {
		const { data, error: err } = await api.GET('/api/v1/tokens');
		loading = false;
		if (data) tokens = data.items;
		else error = errorMessage(err);
	}

	$effect(() => {
		load();
	});

	async function create(event: SubmitEvent) {
		event.preventDefault();
		creating = true;
		error = '';
		const { data, error: err } = await api.POST('/api/v1/tokens', {
			body: { name, expiresInDays: expiresInDays || undefined }
		});
		creating = false;
		if (!data) {
			error = errorMessage(err);
			return;
		}
		created = data;
		copied = false;
		name = '';
		await load();
	}

	async function revoke(token: ApiToken) {
		const { error: err } = await api.DELETE('/api/v1/tokens/{tokenId}', {
			params: { path: { tokenId: token.id } }
		});
		if (err) {
			error = errorMessage(err);
			return;
		}
		if (created?.id === token.id) created = null;
		await load();
	}

	async function copy() {
		if (!created) return;
		await navigator.clipboard.writeText(created.token);
		copied = true;
	}

	function fmt(date: string | null | undefined) {
		return date ? new Date(date).toLocaleString() : '—';
	}
</script>

<h1>API tokens</h1>
<p class="muted">
	Use a token from scripts and CI by sending <code>Authorization: Bearer &lt;token&gt;</code>.
</p>

{#if error}
	<p class="error" role="alert">{error}</p>
{/if}

{#if created}
	<section class="card created">
		<h2>Token “{created.name}” created</h2>
		<p>Copy it now. It won't be shown again.</p>
		<div class="secret">
			<code>{created.token}</code>
			<button class="secondary" onclick={copy}>{copied ? 'Copied' : 'Copy'}</button>
		</div>
	</section>
{/if}

<section class="card">
	<h2>New token</h2>
	<form onsubmit={create} class="new">
		<label>
			Name
			<input bind:value={name} maxlength="100" placeholder="e.g. ci-pipeline" required />
		</label>
		<label>
			Expires in (days)
			<input type="number" bind:value={expiresInDays} min="1" max="3650" placeholder="Never" />
		</label>
		<button type="submit" disabled={creating}>Create token</button>
	</form>
</section>

<section class="card">
	<h2>Your tokens</h2>
	{#if loading}
		<p class="muted">Loading…</p>
	{:else if tokens.length === 0}
		<p class="muted">You have no API tokens.</p>
	{:else}
		<div class="table-wrap">
			<table>
				<thead>
					<tr>
						<th>Name</th>
						<th>Token</th>
						<th>Created</th>
						<th>Expires</th>
						<th>Last used</th>
						<th></th>
					</tr>
				</thead>
				<tbody>
					{#each tokens as token (token.id)}
						<tr>
							<td>{token.name}</td>
							<td><code>{token.prefix}…</code></td>
							<td>{fmt(token.createdAt)}</td>
							<td>{token.expiresAt ? fmt(token.expiresAt) : 'Never'}</td>
							<td>{fmt(token.lastUsedAt)}</td>
							<td><button class="danger" onclick={() => revoke(token)}>Revoke</button></td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>

<style>
	section {
		margin-bottom: 1rem;
	}

	.new {
		display: grid;
		grid-template-columns: 2fr 1fr auto;
		gap: 1rem;
		align-items: end;
	}

	.new label {
		margin: 0;
	}

	.created {
		border-color: var(--ok);
	}

	.secret {
		display: flex;
		gap: 0.75rem;
		align-items: center;
	}

	.secret code {
		flex: 1;
		overflow-wrap: anywhere;
		padding: 0.5rem;
		background: var(--bg);
		border-radius: 6px;
	}

	.table-wrap {
		overflow-x: auto;
	}

	@media (max-width: 700px) {
		.new {
			grid-template-columns: 1fr;
		}
	}
</style>
