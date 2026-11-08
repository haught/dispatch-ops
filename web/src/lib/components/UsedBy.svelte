<script lang="ts">
	import type { JobTemplate } from '#lib/api/client.js';

	/** The job templates that use a resource. */
	let { templates, what }: { templates: JobTemplate[]; what: string } = $props();
</script>

<section class="card">
	<h2>Used by</h2>
	{#if templates.length === 0}
		<p class="muted">No job templates you can see use this {what}.</p>
	{:else}
		<ul>
			{#each templates as t (t.id)}
				<li>
					<a href="/templates/{t.id}">{t.name}</a>
					<span class="muted small">{t.jobType}{t.entrypoint ? ` · ${t.entrypoint}` : ''}</span>
				</li>
			{/each}
		</ul>
	{/if}
</section>

<style>
	ul {
		margin: 0;
		padding-left: 1.25rem;
	}
</style>
