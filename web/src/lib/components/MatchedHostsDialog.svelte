<script lang="ts">
	import { untrack } from 'svelte';
	import { api, errorMessage, type MatchedHost } from '#lib/api/client.js';

	/**
	 * Lists the enabled hosts in an inventory that a limit pattern selects,
	 * as Ansible matches it. Opened over the launch dialog.
	 */
	let {
		inventoryId,
		inventoryName,
		pattern,
		onclose
	}: {
		inventoryId: string;
		inventoryName: string | null;
		pattern: string;
		onclose: () => void;
	} = $props();

	const iid = untrack(() => inventoryId);
	const p = untrack(() => pattern);
	const pageSize = 500;

	let dialog: HTMLDialogElement;
	let search = $state('');
	let hosts = $state<MatchedHost[]>([]);
	let total = $state(0);
	let unmatched = $state<string[]>([]);
	let loading = $state(true);
	let error = $state('');
	let groupName = $state(new Map<string, string>());
	// Responses for an older search are dropped.
	let seq = 0;

	$effect(() => {
		dialog.showModal();
		api
			.GET('/api/v1/inventories/{inventoryId}/groups', {
				params: { path: { inventoryId: iid }, query: { limit: 500 } }
			})
			.then((r) => (groupName = new Map((r.data?.items ?? []).map((g) => [g.id, g.name]))));
	});

	async function load(q: string, offset: number) {
		const mine = ++seq;
		loading = true;
		const { data, error: err } = await api.GET('/api/v1/inventories/{inventoryId}/host-matches', {
			params: {
				path: { inventoryId: iid },
				query: { pattern: p || undefined, search: q || undefined, limit: pageSize, offset }
			}
		});
		if (mine !== seq) return;
		loading = false;
		if (data) {
			hosts = offset ? [...hosts, ...data.items] : data.items;
			total = data.total;
			unmatched = data.unmatched;
			error = '';
		} else {
			error = errorMessage(err);
		}
	}

	$effect(() => {
		const q = search.trim();
		const timer = setTimeout(() => load(q, 0), q ? 250 : 0);
		return () => clearTimeout(timer);
	});

	const plural = (n: number) => `${n} host${n === 1 ? '' : 's'}`;
</script>

<!-- A click outside the panel lands on the dialog itself (its backdrop); Esc closes it too. -->
<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_noninteractive_element_interactions -->
<dialog
	bind:this={dialog}
	aria-labelledby="matched-hosts-title"
	onclose={() => onclose()}
	onclick={(e) => e.target === dialog && dialog.close()}
>
	<div class="panel">
		<header>
			<h2 id="matched-hosts-title">
				{#if p}Hosts matching <code>{p}</code>{:else}All hosts{/if}
			</h2>
		</header>

		<div class="body">
			<p class="muted small">
				Enabled hosts in {#if inventoryName}<strong>{inventoryName}</strong>{:else}the inventory{/if}
				that the limit selects, as Ansible matches it. A playbook's <code>hosts:</code> can narrow them
				further.
			</p>
			{#if unmatched.length}
				<p class="warn small">
					Matched nothing: {unmatched.map((u) => `"${u}"`).join(', ')}
				</p>
			{/if}
			<label>
				Search
				<input bind:value={search} placeholder="Name contains…" />
			</label>
			{#if error}
				<p class="error" role="alert">{error}</p>
			{:else if hosts.length === 0}
				<p class="muted">
					{#if loading}Loading…{:else if search.trim()}No matching hosts contain "{search.trim()}".{:else}No
						hosts match.{/if}
				</p>
			{:else}
				<div class="table-wrap">
					<table>
						<thead>
							<tr>
								<th>Host</th>
								<th>Groups</th>
							</tr>
						</thead>
						<tbody>
							{#each hosts as h (h.id)}
								<tr>
									<td><code>{h.name}</code></td>
									<td class="small">
										{h.groupIds.map((g) => groupName.get(g) ?? '?').join(', ')}
									</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			{/if}
		</div>

		<footer>
			<span class="muted small">
				{#if hosts.length < total}Showing {hosts.length} of {plural(total)}{:else}{plural(
						total
					)}{/if}
			</span>
			<div class="actions">
				{#if hosts.length < total}
					<button
						type="button"
						class="secondary"
						disabled={loading}
						onclick={() => load(search.trim(), hosts.length)}
					>
						{loading ? 'Loading…' : 'Load more'}
					</button>
				{/if}
				<button type="button" class="secondary" onclick={() => dialog.close()}>Close</button>
			</div>
		</footer>
	</div>
</dialog>

<style>
	dialog {
		width: min(640px, calc(100vw - 2rem));
		max-height: calc(100vh - 2rem);
		padding: 0;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--surface);
		color: var(--text);
		box-shadow: 0 10px 40px rgb(0 0 0 / 0.25);
	}

	dialog::backdrop {
		background: rgb(0 0 0 / 0.3);
	}

	.panel {
		display: flex;
		flex-direction: column;
		max-height: calc(100vh - 2rem);
	}

	header,
	footer {
		padding: 1rem 1.25rem;
	}

	header {
		border-bottom: 1px solid var(--border);
	}

	header h2 {
		margin: 0;
		overflow-wrap: anywhere;
	}

	.body {
		padding: 0.5rem 1.25rem 1rem;
		overflow-y: auto;
	}

	footer {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 1rem;
		border-top: 1px solid var(--border);
	}

	.actions {
		display: flex;
		gap: 0.5rem;
	}

	.warn {
		color: var(--warn);
	}

	td code {
		overflow-wrap: anywhere;
	}
</style>
