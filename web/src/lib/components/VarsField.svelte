<script lang="ts">
	import type { Snippet } from 'svelte';
	import { varsError } from '#lib/format.js';

	/**
	 * A textarea for variables as a JSON object, flagging invalid JSON as it
	 * is typed. aside renders at the end of the label row.
	 */
	let {
		value = $bindable(''),
		label = 'Variables',
		disabled = false,
		aside
	}: { value: string; label?: string; disabled?: boolean; aside?: Snippet } = $props();

	const id = $props.id();
	const problem = $derived(varsError(value));
</script>

<div class="vars">
	<div class="head">
		<label for={id}>{label} <span class="muted small">JSON object</span></label>
		{@render aside?.()}
	</div>
	<textarea
		{id}
		bind:value
		class="code"
		rows={Math.min(16, Math.max(4, value.split('\n').length + 1))}
		spellcheck="false"
		placeholder={'{\n  "ansible_user": "deploy"\n}'}
		{disabled}></textarea>
	{#if problem}<span class="error small">{problem}</span>{/if}
</div>

<style>
	.vars {
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

	textarea {
		display: block;
		width: 100%;
		margin-top: 0.25rem;
	}
</style>
