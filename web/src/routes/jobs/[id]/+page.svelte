<script lang="ts">
	import { page } from '$app/state';
	import { tick } from 'svelte';
	import { api, errorMessage, finishedStatuses, type Job, type JobEvent } from '#lib/api/client.js';
	import StatusBadge from '#lib/components/StatusBadge.svelte';
	import { fmtDuration, fmtTime } from '#lib/format.js';

	let job = $state<Job | null>(null);
	// Raw: the log can be long, and lines never change once added.
	let lines = $state.raw<JobEvent[]>([]);
	let live = $state(false);
	let error = $state('');
	let canceling = $state(false);

	let logEl = $state<HTMLElement | null>(null);
	let follow = $state(true);
	// Ticks once a second so the running duration updates.
	let now = $state(Date.now());

	const finished = $derived(job ? finishedStatuses.includes(job.status) : false);

	$effect(() => {
		const id = page.params.id;
		if (!id) return;
		job = null;
		lines = [];
		error = '';
		let source: EventSource | null = null;
		let pending: JobEvent[] = [];
		let frame = 0;
		let closed = false;

		// Batch incoming lines into one update per animation frame.
		const flush = () => {
			frame = 0;
			lines = lines.concat(pending);
			pending = [];
			scrollIfFollowing();
		};

		api
			.GET('/api/v1/jobs/{jobId}', { params: { path: { jobId: id } } })
			.then(({ data, error: err }) => {
				if (closed) return;
				if (!data) {
					error = errorMessage(err);
					return;
				}
				job = data;
				source = new EventSource(`/api/v1/jobs/${id}/stream`);
				live = true;
				source.addEventListener('status', (e) => {
					job = JSON.parse(e.data);
				});
				source.addEventListener('job_event', (e) => {
					pending.push(JSON.parse(e.data));
					if (!frame) frame = requestAnimationFrame(flush);
				});
				source.addEventListener('end', () => {
					source?.close();
					live = false;
				});
				source.onerror = () => {
					// The browser reconnects by itself (resuming with Last-Event-ID)
					// unless the server refused the stream.
					if (source?.readyState === EventSource.CLOSED) {
						live = false;
						error = 'Lost the live connection. Reload to try again.';
					}
				};
			});

		const timer = setInterval(() => (now = Date.now()), 1000);
		return () => {
			closed = true;
			source?.close();
			if (frame) cancelAnimationFrame(frame);
			clearInterval(timer);
		};
	});

	async function scrollIfFollowing() {
		if (!follow) return;
		await tick();
		logEl?.scrollTo({ top: logEl.scrollHeight });
	}

	function onScroll() {
		if (!logEl) return;
		follow = logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 40;
	}

	async function cancel() {
		if (!job) return;
		canceling = true;
		const { data, error: err } = await api.POST('/api/v1/jobs/{jobId}/cancel', {
			params: { path: { jobId: job.id } }
		});
		canceling = false;
		if (data) job = data;
		else error = errorMessage(err);
	}

	function duration(j: Job) {
		void now;
		return fmtDuration(j.startedAt, j.finishedAt);
	}
</script>

{#if job}
	<div class="header">
		<h1>{job.name}</h1>
		<StatusBadge status={job.status} />
		{#if live}<span class="live" title="Receiving output live">● live</span>{/if}
		<span class="spacer"></span>
		{#if !finished}
			<button class="danger" onclick={cancel} disabled={canceling || job.cancelRequested}>
				{job.cancelRequested ? 'Canceling…' : 'Cancel'}
			</button>
		{/if}
	</div>

	{#if job.failureReason}
		<p class="reason" class:error={job.status !== 'canceled'}>{job.failureReason}</p>
	{/if}
	{#if error}
		<p class="error" role="alert">{error}</p>
	{/if}

	<section class="card details">
		<dl>
			<dt>Image</dt>
			<dd><code>{job.config.executionEnvironment.image}</code></dd>
			<dt>Command</dt>
			<dd><code>{job.config.command.join(' ')}</code></dd>
			<dt>Launched by</dt>
			<dd>{job.launchedBy?.username ?? '—'} ({job.launchType})</dd>
			<dt>Created</dt>
			<dd>{fmtTime(job.createdAt)}</dd>
			<dt>Started</dt>
			<dd>{fmtTime(job.startedAt)}</dd>
			<dt>Duration</dt>
			<dd>{duration(job)}</dd>
			{#if job.exitCode !== null}
				<dt>Exit code</dt>
				<dd>{job.exitCode}</dd>
			{/if}
		</dl>
	</section>

	<section class="card log-card">
		<pre class="log" bind:this={logEl} onscroll={onScroll}>{#each lines as line (line.seq)}<span
					class="line {line.type}">{line.stdout}</span
				>{'\n'}{:else}<span class="muted">{finished ? 'No output.' : 'Waiting for output…'}</span
				>{/each}</pre>
		{#if !follow && live}
			<button class="secondary jump" onclick={() => ((follow = true), scrollIfFollowing())}>
				Follow output ↓
			</button>
		{/if}
	</section>
{:else if error}
	<p class="error" role="alert">{error}</p>
	<p><a href="/jobs">Back to jobs</a></p>
{:else}
	<p class="muted">Loading…</p>
{/if}

<style>
	.header {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		margin-bottom: 1rem;
	}

	.header h1 {
		margin: 0;
	}

	.spacer {
		flex: 1;
	}

	.live {
		color: var(--ok);
		font-size: 0.85rem;
	}

	.reason {
		margin-top: 0;
	}

	.details {
		margin-bottom: 1rem;
	}

	dl {
		display: grid;
		grid-template-columns: max-content 1fr;
		gap: 0.25rem 1rem;
		margin: 0;
	}

	dt {
		color: var(--muted);
	}

	dd {
		margin: 0;
		overflow-wrap: anywhere;
	}

	.log-card {
		position: relative;
		padding: 0;
	}

	.log {
		margin: 0;
		padding: 1rem;
		height: 60vh;
		overflow: auto;
		font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
		font-size: 0.85rem;
		line-height: 1.45;
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
