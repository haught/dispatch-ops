// The part of a job's log the job page holds. Lines are parsed once, when
// added, and kept in blocks: a full block never changes, so adding lines
// re-renders only the last one, and the browser can skip laying out the
// blocks that are scrolled out of view.

import type { JobEvent } from '#lib/api/client.js';
import { parseAnsi, type AnsiSegment } from '#lib/ansi.js';

/** Lines per block. */
export const BLOCK = 500;
/** Lines kept while following the end of the log; older ones are dropped. */
export const MAX_LINES = 50_000;
/** Lines fetched at a time when opening the log or loading earlier lines. */
export const PAGE = 2_000;

export type LogLine = { seq: number; runner: boolean; segments: AnsiSegment[] };

/** Events that belong in the log: output lines, runner messages, and
 * structured events with output. */
export function isLogEvent(e: JobEvent): boolean {
	return e.type === 'stdout' || e.type === 'runner' || e.stdout !== '';
}

function toLine(e: JobEvent): LogLine {
	return { seq: e.seq, runner: e.type === 'runner', segments: parseAnsi(e.stdout) };
}

export class JobLog {
	/** The lines, in blocks; only the last may still grow. */
	blocks = $state.raw<LogLine[][]>([]);
	/** Whether the log has lines before the first one held. */
	hasEarlier = $state(false);

	get firstSeq(): number {
		return this.blocks[0]?.[0]?.seq ?? 0;
	}

	get lastSeq(): number {
		const last = this.blocks.at(-1);
		return last?.at(-1)?.seq ?? 0;
	}

	get lineCount(): number {
		let n = 0;
		for (const b of this.blocks) n += b.length;
		return n;
	}

	clear() {
		this.blocks = [];
		this.hasEarlier = false;
	}

	/** Adds events after the last line. */
	append(events: JobEvent[]) {
		const lines = events.filter((e) => e.seq > this.lastSeq && isLogEvent(e)).map(toLine);
		if (lines.length === 0) return;
		const blocks = this.blocks.slice();
		// A new array for the block that grows, so it re-renders.
		let last: LogLine[] = [];
		const tail = blocks.at(-1);
		if (tail && tail.length < BLOCK) last = blocks[blocks.length - 1] = tail.slice();
		else blocks.push(last);
		for (const line of lines) {
			if (last.length >= BLOCK) blocks.push((last = []));
			last.push(line);
		}
		this.blocks = blocks;
	}

	/** Drops the oldest blocks past MAX_LINES (while following the end, so
	 * that a long log doesn't take ever more memory). */
	trim() {
		let total = this.lineCount;
		if (total <= MAX_LINES) return;
		const blocks = this.blocks.slice();
		while (total > MAX_LINES && blocks.length > 1) total -= blocks.shift()!.length;
		this.blocks = blocks;
		this.hasEarlier = true;
	}

	/** Adds events before the first line; more says whether there are yet
	 * earlier ones. */
	prepend(events: JobEvent[], more: boolean) {
		const first = this.firstSeq;
		const lines = events.filter((e) => (first === 0 || e.seq < first) && isLogEvent(e)).map(toLine);
		const blocks: LogLine[][] = [];
		for (let i = 0; i < lines.length; i += BLOCK) blocks.push(lines.slice(i, i + BLOCK));
		this.blocks = [...blocks, ...this.blocks];
		this.hasEarlier = more;
	}
}
