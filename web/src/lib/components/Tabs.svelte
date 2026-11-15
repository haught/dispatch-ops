<script lang="ts">
	import { activeTab, selectTab, tabHref, type Tab } from '#lib/tabs.svelte.js';

	/**
	 * A page's tab bar. The ?tab= query parameter holds the selected tab, so
	 * tabs can be linked to and survive a reload. The page shows each tab's
	 * content in a `role="tabpanel"` element with the ID `panel-<tab ID>`,
	 * hidden unless it is the active one. With fewer than two tabs there is
	 * no bar.
	 */
	let { tabs, label }: { tabs: Tab[]; label: string } = $props();

	const active = $derived(activeTab(tabs));
	let bar = $state<HTMLElement>();

	async function onkeydown(event: KeyboardEvent) {
		const i = tabs.findIndex((t) => t.id === active);
		let next: number;
		if (event.key === 'ArrowRight') next = (i + 1) % tabs.length;
		else if (event.key === 'ArrowLeft') next = (i - 1 + tabs.length) % tabs.length;
		else if (event.key === 'Home') next = 0;
		else if (event.key === 'End') next = tabs.length - 1;
		else return;
		event.preventDefault();
		await selectTab(tabs, tabs[next].id);
		bar?.querySelector<HTMLElement>('[aria-selected="true"]')?.focus();
	}
</script>

{#if tabs.length > 1}
	<div class="tabs" role="tablist" aria-label={label} bind:this={bar}>
		{#each tabs as t (t.id)}
			<a
				role="tab"
				id="tab-{t.id}"
				href={tabHref(tabs, t.id)}
				aria-selected={t.id === active}
				aria-controls="panel-{t.id}"
				tabindex={t.id === active ? 0 : -1}
				data-sveltekit-reset="false"
				{onkeydown}
			>
				{t.label}
				{#if t.count !== undefined}<span class="count">{t.count}</span>{/if}
			</a>
		{/each}
	</div>
{/if}

<style>
	.tabs {
		display: flex;
		gap: 0.25rem;
		margin: 0.75rem 0 1rem;
		border-bottom: 1px solid var(--border);
		overflow-x: auto;
		scrollbar-width: thin;
	}

	a {
		display: flex;
		align-items: center;
		gap: 0.4rem;
		padding: 0.5rem 0.75rem;
		margin-bottom: -1px;
		border-bottom: 2px solid transparent;
		color: var(--muted);
		text-decoration: none;
		white-space: nowrap;
	}

	a:hover {
		color: var(--text);
	}

	a[aria-selected='true'] {
		color: var(--text);
		border-bottom-color: var(--accent);
		font-weight: 600;
	}

	a:focus-visible {
		outline: 2px solid var(--accent);
		outline-offset: -2px;
		border-radius: 4px;
	}

	.count {
		padding: 0 0.45rem;
		border-radius: 999px;
		background: var(--border);
		color: var(--muted);
		font-size: 0.8rem;
		font-weight: 400;
	}
</style>
