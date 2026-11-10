<script lang="ts">
	import type { Snippet } from 'svelte';
	import type { OverridableField } from '#lib/api/client.js';
	import PromptToggle from './PromptToggle.svelte';

	/**
	 * A form field whose label row has a "Prompt on launch" checkbox for
	 * field. children renders the control, given the id its label points at.
	 */
	let {
		label,
		hint = '',
		field,
		prompts = $bindable([]),
		children
	}: {
		label: string;
		hint?: string;
		field: OverridableField;
		prompts: OverridableField[];
		children: Snippet<[string]>;
	} = $props();

	const id = $props.id();
</script>

<div class="prompt-field">
	<div class="head">
		<label for={id}
			>{label}
			{#if hint}<span class="muted small">{hint}</span>{/if}</label
		>
		<PromptToggle {field} bind:prompts />
	</div>
	{@render children(id)}
</div>

<style>
	.prompt-field {
		margin-bottom: 0.75rem;
	}

	.head {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		justify-content: space-between;
		gap: 0 0.75rem;
	}

	.head label {
		margin: 0;
	}

	.prompt-field > :global(:is(input, select, textarea)) {
		display: block;
		width: 100%;
		margin-top: 0.25rem;
		font-weight: normal;
	}
</style>
