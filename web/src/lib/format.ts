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

/** Variables as editable JSON; blank when there are none. */
export function fmtVars(vars: Record<string, unknown> | null | undefined): string {
	return vars && Object.keys(vars).length ? JSON.stringify(vars, null, 2) : '';
}

/** Why text isn't a valid variables object, or '' if it is (blank counts as none). */
export function varsError(text: string): string {
	if (!text.trim()) return '';
	try {
		const v: unknown = JSON.parse(text);
		if (v === null || typeof v !== 'object' || Array.isArray(v)) {
			return 'Variables must be a JSON object, e.g. {"ansible_user": "deploy"}';
		}
		return '';
	} catch (e) {
		return `Not valid JSON: ${e instanceof Error ? e.message : String(e)}`;
	}
}

/** Parses variables typed as JSON (check varsError first); blank gives {}. */
export function parseVars(text: string): Record<string, unknown> {
	return text.trim() ? (JSON.parse(text) as Record<string, unknown>) : {};
}

/*
 * A command or argument list is edited as one item per line, or as a JSON
 * array of strings when an item has a line break (e.g. the script after
 * `bash -c`) or would otherwise be lost (blank, or the first starts with "[").
 */

/** A command or argument list as editable text. */
export function fmtArgv(argv: string[]): string {
	const asJSON =
		argv.some((a) => a.includes('\n') || !a.trim()) || argv[0]?.trimStart().startsWith('[');
	return asJSON ? JSON.stringify(argv, null, 2) : argv.join('\n');
}

/** Why text isn't a valid command or argument list, or '' if it is. */
export function argvError(text: string): string {
	if (!text.trimStart().startsWith('[')) return '';
	try {
		const v: unknown = JSON.parse(text);
		if (!Array.isArray(v) || v.some((a) => typeof a !== 'string')) {
			return 'A JSON list must be an array of strings, e.g. ["bash", "-c", "echo hi"]';
		}
		return '';
	} catch (e) {
		return `Not valid JSON: ${e instanceof Error ? e.message : String(e)}`;
	}
}

/** Parses a command or argument list typed as text (check argvError first). */
export function parseArgv(text: string): string[] {
	if (text.trimStart().startsWith('[')) return JSON.parse(text) as string[];
	return text
		.split('\n')
		.map((l) => l.replace(/\r$/, ''))
		.filter((l) => l.trim());
}

/** A number of seconds as a duration, e.g. "1h 00m". */
export function fmtSeconds(seconds: number): string {
	return fmtDuration(new Date(0).toISOString(), new Date(seconds * 1000).toISOString());
}

/** How long ago (or, for a future date, how soon) a date is, e.g. "5m ago" or "in 2h". */
export function fmtRelative(date: string | null | undefined, now = Date.now()): string {
	if (!date) return '—';
	const diff = new Date(date).getTime() - now;
	const s = Math.round(Math.abs(diff) / 1000);
	let text: string;
	if (s < 45) text = `${s}s`;
	else if (s < 3600) text = `${Math.round(s / 60)}m`;
	else if (s < 86400) text = `${Math.round(s / 3600)}h`;
	else text = `${Math.round(s / 86400)}d`;
	if (s < 5) return 'just now';
	return diff < 0 ? `${text} ago` : `in ${text}`;
}

/** A 0–1 ratio as a percentage, e.g. "97.5%", or a dash. */
export function fmtPercent(ratio: number | null | undefined): string {
	if (ratio == null) return '—';
	const pct = ratio * 100;
	return `${pct >= 99.95 || pct === 0 ? pct.toFixed(0) : pct.toFixed(1)}%`;
}
