/** A date-time for display, or a dash. */
export function fmtTime(date: string | null | undefined): string {
	return date ? new Date(date).toLocaleString() : '—';
}

/** Elapsed time between two instants (end defaults to now), e.g. "1m 05s". */
export function fmtDuration(start: string | null | undefined, end?: string | null): string {
	if (!start) return '—';
	const ms = (end ? new Date(end) : new Date()).getTime() - new Date(start).getTime();
	const s = Math.max(0, Math.round(ms / 1000));
	const h = Math.floor(s / 3600);
	const m = Math.floor((s % 3600) / 60);
	const sec = String(s % 60).padStart(2, '0');
	if (h) return `${h}h ${String(m).padStart(2, '0')}m`;
	if (m) return `${m}m ${sec}s`;
	return `${s % 60}s`;
}
