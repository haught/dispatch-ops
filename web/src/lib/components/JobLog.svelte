<script lang="ts">
	import { tick } from 'svelte';
	import type { JobLog } from '#lib/joblog.svelte.js';

	/**
	 * A job's log, as much of it as `log` holds, following new lines while
	 * scrolled to the end. Earlier lines load on request.
	 */
	let {
		log,
		jobId,
		live,
		finished,
		visible,
		loadEarlier
	}: {
		log: JobLog;
		jobId: string;
		live: boolean;
		finished: boolean;
		/** Whether the log's tab shows: it can't scroll while hidden. */
		visible: boolean;
		loadEarlier: () => Promise<void>;
	} = $props();

	let el = $state<HTMLElement | null>(null);
	let follow = $state(true);
	let loading = $state(false);

	async function scrollIfFollowing() {
		if (!follow || !visible) return;
		await tick();
		el?.scrollTo({ top: el.scrollHeight });
	}

	$effect(() => {
		void log.blocks;
		void visible;
		if (follow) log.trim();
		scrollIfFollowing();
	});

	function onScroll() {
		if (!el || !visible) return;
		follow = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
	}

	async function earlier() {
		if (!el) return;
		loading = true;
		const before = el.scrollHeight;
		try {
			await loadEarlier();
			await tick();
			// Keep the lines that were showing in place.
			el.scrollTop += el.scrollHeight - before;
		} finally {
			loading = false;
		}
	}
</script>

<section class="card log-card">
	<div class="log-bar">
		{#if log.hasEarlier}
			<button class="secondary small" onclick={earlier} disabled={loading}>
				{loading ? 'Loading…' : 'Load earlier output'}
			</button>
		{/if}
		<span class="spacer"></span>
		<a class="small" href="/api/v1/jobs/{jobId}/output?ansi=false" download>Download log</a>
	</div>
	<!-- Focusable so that the log can be scrolled with the keyboard. -->
	<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
	<div
		class="log"
		bind:this={el}
		onscroll={onScroll}
		tabindex="0"
		role="region"
		aria-label="Job output"
	>
		{#each log.blocks as block (block[0].seq)}
			<div class="block" style="contain-intrinsic-size: auto calc({block.length} * 1lh)">
				{#each block as line (line.seq)}
					<div class="line" class:runner={line.runner}>
						{#each line.segments as seg, i (i)}<span class={seg.cls}>{seg.text}</span>{/each}
					</div>
				{/each}
			</div>
		{:else}
			<span class="muted">{finished ? 'No output.' : 'Waiting for output…'}</span>
		{/each}
	</div>
	{#if !follow && live}
		<button class="secondary jump" onclick={() => ((follow = true), scrollIfFollowing())}>
			Follow output ↓
		</button>
	{/if}
</section>

<style>
	.log-card {
		position: relative;
		padding: 0;
	}

	.log-bar {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		padding: 0.5rem 1rem;
		border-bottom: 1px solid var(--border);
	}

	.spacer {
		flex: 1;
	}

	.small {
		font-size: 0.85rem;
	}

	button.small {
		padding: 0.25rem 0.6rem;
	}

	.log {
		margin: 0;
		padding: 1rem;
		height: 60vh;
		overflow: auto;
		font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
		font-size: 0.85rem;
		line-height: 1.45;
	}

	/* Blocks out of view are not laid out or painted. */
	.block {
		content-visibility: auto;
	}

	.line {
		min-height: 1lh;
		white-space: pre-wrap;
		overflow-wrap: anywhere;
	}

	.line.runner {
		color: var(--muted);
		font-style: italic;
	}

	.jump {
		position: absolute;
		right: 1rem;
		bottom: 1rem;
		background: var(--surface);
	}
</style>
