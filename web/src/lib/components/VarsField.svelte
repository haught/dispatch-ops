<script lang="ts">
	import { varsError } from '#lib/format.js';

	/** A textarea for variables as a JSON object, flagging invalid JSON as it is typed. */
	let {
		value = $bindable(''),
		label = 'Variables',
		disabled = false
	}: { value: string; label?: string; disabled?: boolean } = $props();

	const problem = $derived(varsError(value));
</script>

<label>
	{label} <span class="muted small">JSON object</span>
	<textarea
		bind:value
		class="code"
		rows={Math.min(16, Math.max(4, value.split('\n').length + 1))}
		spellcheck="false"
		placeholder={'{\n  "ansible_user": "deploy"\n}'}
		{disabled}></textarea>
	{#if problem}<span class="error small">{problem}</span>{/if}
</label>
