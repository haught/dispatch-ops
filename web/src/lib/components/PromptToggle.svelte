<script lang="ts">
	import type { OverridableField } from '#lib/api/client.js';

	/** A "Prompt on launch" checkbox that adds field to (or removes it from) prompts. */
	let { field, prompts = $bindable([]) }: { field: OverridableField; prompts: OverridableField[] } =
		$props();
</script>

<label class="prompt small">
	<input type="checkbox" bind:group={prompts} value={field} /> Prompt on launch
</label>

<style>
	.prompt {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
		margin: 0;
		font-weight: normal;
		color: var(--muted);
		white-space: nowrap;
	}

	.prompt input {
		margin: 0;
	}

	.prompt:has(input:checked) {
		color: var(--accent);
	}
</style>
