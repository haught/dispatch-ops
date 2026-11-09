<script lang="ts" module>
	let nextId = 0;
</script>

<script lang="ts">
	import type { JobSeriesBucket } from '#lib/api/client.js';

	/**
	 * Jobs per hour or day as stacked bars: successful at the base, then
	 * failed (failed or error, hatched so it doesn't rely on red vs green),
	 * then other (canceled or still active). Hovering a bar shows its counts;
	 * the same numbers are in a table below.
	 */
	let { series, bucket }: { series: JobSeriesBucket[]; bucket: 'hour' | 'day' } = $props();

	const id = `activity-${nextId++}`;
	const height = 180;
	const m = { top: 8, right: 4, bottom: 22, left: 30 };
	const gap = 2;

	let width = $state(600);
	let hovered = $state<number | null>(null);

	const rows = $derived(
		series.map((b) => ({
			start: new Date(b.start),
			successful: b.successful,
			failed: b.failed + b.error,
			other: b.canceled + b.active,
			total: b.successful + b.failed + b.error + b.canceled + b.active
		}))
	);

	// The y axis tops out at a round number with two even steps.
	const top = $derived.by(() => {
		const max = Math.max(1, ...rows.map((r) => r.total));
		const raw = max / 2;
		const mag = 10 ** Math.floor(Math.log10(raw));
		const step =
			[1, 2, 2.5, 5, 10].map((f) => f * mag).find((s) => s >= raw && Number.isInteger(s)) ??
			Math.ceil(raw);
		return step * 2;
	});
	const ticks = $derived([0, top / 2, top]);

	const plotW = $derived(Math.max(0, width - m.left - m.right));
	const plotH = height - m.top - m.bottom;
	const slot = $derived(rows.length ? plotW / rows.length : 0);
	const barW = $derived(Math.max(2, Math.min(28, slot * 0.7)));
	const y = (v: number) => m.top + plotH - (v / top) * plotH;

	const fmtLabel = (d: Date) =>
		bucket === 'hour'
			? d.toLocaleTimeString(undefined, { hour: 'numeric' })
			: d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
	const fmtFull = (d: Date) =>
		bucket === 'hour'
			? d.toLocaleString(undefined, { weekday: 'short', hour: 'numeric', minute: '2-digit' })
			: d.toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric' });

	// Label every nth bar so labels stay ~56px apart.
	const labelEvery = $derived(Math.max(1, Math.ceil(56 / Math.max(slot, 1))));

	type Seg = { key: 'successful' | 'failed' | 'other'; y: number; h: number; top: boolean };
	function segments(r: (typeof rows)[number]): Seg[] {
		const out: Seg[] = [];
		let base = 0;
		const keys = (['successful', 'failed', 'other'] as const).filter((k) => r[k] > 0);
		keys.forEach((k, i) => {
			const y0 = y(base);
			const y1 = y(base + r[k]);
			base += r[k];
			const isTop = i === keys.length - 1;
			// A 2px surface gap separates stacked segments.
			const h = Math.max(1, y0 - y1 - (i > 0 ? gap : 0));
			out.push({ key: k, y: y1, h, top: isTop });
		});
		return out;
	}

	// A bar segment with rounded top corners (only the topmost is rounded).
	function topRounded(x: number, yy: number, w: number, h: number): string {
		const r = Math.min(4, w / 2, h);
		return `M${x},${yy + h} V${yy + r} Q${x},${yy} ${x + r},${yy} H${x + w - r} Q${x + w},${yy} ${x + w},${yy + r} V${yy + h} Z`;
	}

	const legend = [
		{ key: 'successful', label: 'Successful' },
		{ key: 'failed', label: 'Failed or error' },
		{ key: 'other', label: 'Canceled or active' }
	] as const;

	const tip = $derived(hovered === null ? null : rows[hovered]);
	const tipLeft = $derived(
		hovered === null ? 0 : Math.min(Math.max(m.left + slot * (hovered + 0.5), 80), width - 80)
	);

	function onMove(e: PointerEvent) {
		const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
		const i = Math.floor((e.clientX - rect.left - m.left) / slot);
		hovered = i >= 0 && i < rows.length ? i : null;
	}
</script>

<div class="legend" aria-hidden="true">
	{#each legend as l (l.key)}
		<span class="item">
			<svg width="12" height="12" viewBox="0 0 12 12">
				<rect
					width="12"
					height="12"
					rx="2"
					class="seg {l.key}"
					fill={l.key === 'failed' ? `url(#${id}-hatch)` : undefined}
				/>
			</svg>
			{l.label}
		</span>
	{/each}
</div>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
	class="chart"
	bind:clientWidth={width}
	onpointermove={onMove}
	onpointerleave={() => (hovered = null)}
>
	<svg
		{width}
		{height}
		role="img"
		aria-label="Jobs per {bucket}, by outcome. The table below has the numbers."
	>
		<defs>
			<pattern
				id="{id}-hatch"
				patternUnits="userSpaceOnUse"
				width="5"
				height="5"
				patternTransform="rotate(45)"
			>
				<rect width="5" height="5" class="hatch-bg" />
				<line x1="0" y1="0" x2="0" y2="5" class="hatch-line" />
			</pattern>
		</defs>

		{#each ticks as t (t)}
			<line
				x1={m.left}
				x2={width - m.right}
				y1={y(t)}
				y2={y(t)}
				class="grid"
				class:base={t === 0}
			/>
			<text x={m.left - 6} y={y(t)} dy="0.32em" text-anchor="end" class="axis">{t}</text>
		{/each}

		{#each rows as r, i (r.start.getTime())}
			{@const x = m.left + slot * i + (slot - barW) / 2}
			{#if hovered === i}
				<rect x={m.left + slot * i} y={m.top} width={slot} height={plotH} class="hover-band" />
			{/if}
			{#each segments(r) as s (s.key)}
				{#if s.top}
					<path
						d={topRounded(x, s.y, barW, s.h)}
						class="seg {s.key}"
						fill={s.key === 'failed' ? `url(#${id}-hatch)` : undefined}
					/>
				{:else}
					<rect
						{x}
						y={s.y}
						width={barW}
						height={s.h}
						class="seg {s.key}"
						fill={s.key === 'failed' ? `url(#${id}-hatch)` : undefined}
					/>
				{/if}
			{/each}
			{#if i % labelEvery === (rows.length - 1) % labelEvery}
				<text x={m.left + slot * (i + 0.5)} y={height - 6} text-anchor="middle" class="axis"
					>{fmtLabel(r.start)}</text
				>
			{/if}
		{/each}
	</svg>

	{#if tip}
		<div class="tooltip" style:left="{tipLeft}px" role="status">
			<div class="tip-title">{fmtFull(tip.start)}</div>
			<div class="tip-row"><span>Successful</span><span>{tip.successful}</span></div>
			<div class="tip-row"><span>Failed or error</span><span>{tip.failed}</span></div>
			<div class="tip-row"><span>Canceled or active</span><span>{tip.other}</span></div>
			<div class="tip-row total"><span>Total</span><span>{tip.total}</span></div>
		</div>
	{/if}
</div>

<details>
	<summary class="muted small">Show as table</summary>
	<div class="table-wrap">
		<table class="small">
			<thead>
				<tr>
					<th>{bucket === 'hour' ? 'Hour' : 'Day'}</th>
					<th>Successful</th>
					<th>Failed or error</th>
					<th>Canceled or active</th>
					<th>Total</th>
				</tr>
			</thead>
			<tbody>
				{#each rows as r (r.start.getTime())}
					<tr>
						<td>{fmtFull(r.start)}</td>
						<td>{r.successful}</td>
						<td>{r.failed}</td>
						<td>{r.other}</td>
						<td>{r.total}</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
</details>

<style>
	.legend {
		display: flex;
		flex-wrap: wrap;
		gap: 0.25rem 1rem;
		font-size: 0.8rem;
		color: var(--muted);
		margin-bottom: 0.5rem;
	}

	.item {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
	}

	.chart {
		position: relative;
		width: 100%;
	}

	svg {
		display: block;
		overflow: visible;
	}

	.grid {
		stroke: var(--border);
		stroke-width: 1;
		stroke-dasharray: 2 3;
	}

	.grid.base {
		stroke-dasharray: none;
	}

	.axis {
		fill: var(--muted);
		font-size: 11px;
		font-variant-numeric: tabular-nums;
	}

	.seg.successful {
		fill: var(--ok);
	}

	.seg.other {
		fill: var(--muted);
		opacity: 0.55;
	}

	.hatch-bg {
		fill: var(--danger);
	}

	.hatch-line {
		stroke: var(--surface);
		stroke-width: 1.6;
		opacity: 0.55;
	}

	.hover-band {
		fill: var(--border);
		opacity: 0.45;
	}

	.tooltip {
		position: absolute;
		top: 0;
		transform: translate(-50%, -100%);
		background: var(--surface);
		border: 1px solid var(--border);
		border-radius: 6px;
		padding: 0.4rem 0.6rem;
		font-size: 0.8rem;
		pointer-events: none;
		box-shadow: 0 4px 14px rgb(0 0 0 / 12%);
		min-width: 150px;
		z-index: 2;
	}

	.tip-title {
		font-weight: 600;
		margin-bottom: 0.2rem;
	}

	.tip-row {
		display: flex;
		justify-content: space-between;
		gap: 1rem;
		font-variant-numeric: tabular-nums;
	}

	.tip-row.total {
		border-top: 1px solid var(--border);
		margin-top: 0.2rem;
		padding-top: 0.2rem;
		font-weight: 600;
	}

	details {
		margin-top: 0.5rem;
	}

	summary {
		cursor: pointer;
	}

	details table {
		margin-top: 0.5rem;
	}

	details td,
	details th {
		padding: 0.25rem 0.5rem;
		font-variant-numeric: tabular-nums;
	}
</style>
