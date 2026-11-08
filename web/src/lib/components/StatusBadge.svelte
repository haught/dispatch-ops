<script lang="ts">
	import type { JobHostSummary, JobStatus } from '#lib/api/client.js';

	/** A job status, or a host's result in an Ansible job. */
	let { status }: { status: JobStatus | JobHostSummary['status'] } = $props();
</script>

<span class="badge {status}">{status}</span>

<style>
	.badge {
		display: inline-block;
		padding: 0.1rem 0.5rem;
		border-radius: 999px;
		font-size: 0.8rem;
		font-weight: 600;
		border: 1px solid currentColor;
		color: var(--muted);
		white-space: nowrap;
	}

	.running,
	.waiting {
		color: var(--accent);
	}

	.successful,
	.ok {
		color: var(--ok);
	}

	.changed {
		color: var(--warn);
	}

	.failed,
	.error,
	.unreachable {
		color: var(--danger);
	}
</style>
