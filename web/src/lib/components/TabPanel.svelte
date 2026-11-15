<script lang="ts">
	import type { Snippet } from 'svelte';

	/**
	 * The content of one tab of a Tabs bar. It stays mounted while another tab
	 * shows, so forms keep what was typed and live data keeps updating.
	 */
	let { id, active, children }: { id: string; active: string; children: Snippet } = $props();
</script>

<div role="tabpanel" id="panel-{id}" aria-labelledby="tab-{id}" hidden={active !== id}>
	{@render children()}
</div>
