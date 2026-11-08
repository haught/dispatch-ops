<script lang="ts">
	import type { JobStatus } from '#lib/api/client.js';

	let { status }: { status: JobStatus } = $props();
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

	.successful {
		color: var(--ok);
	}

	.failed,
	.error {
		color: var(--danger);
	}
</style>
