// Turns text with ANSI escape sequences (as in colored terminal output)
// into segments with CSS classes. Only SGR sequences (ESC[…m) are styled;
// other escape sequences are dropped.

export type AnsiSegment = { text: string; cls: string };

const names = ['black', 'red', 'green', 'yellow', 'blue', 'magenta', 'cyan', 'white'];

// CSI (ESC[ params final) or OSC (ESC] … BEL or ESC\), or a lone ESC + char.
const escape = /\x1b(?:\[([0-9;?]*)([@-~])|\][^\x07\x1b]*(?:\x07|\x1b\\)?|[@-_]?)/g;

type Style = {
	fg: string;
	bg: string;
	bold: boolean;
	dim: boolean;
	italic: boolean;
	underline: boolean;
};

const plain = (): Style => ({
	fg: '',
	bg: '',
	bold: false,
	dim: false,
	italic: false,
	underline: false
});

function classes(s: Style): string {
	const c: string[] = [];
	if (s.fg) c.push(`ansi-fg-${s.fg}`);
	if (s.bg) c.push(`ansi-bg-${s.bg}`);
	if (s.bold) c.push('ansi-bold');
	if (s.dim) c.push('ansi-dim');
	if (s.italic) c.push('ansi-italic');
	if (s.underline) c.push('ansi-underline');
	return c.join(' ');
}

function apply(s: Style, params: string) {
	const codes = params === '' ? [0] : params.split(';').map((p) => Number(p) || 0);
	for (let i = 0; i < codes.length; i++) {
		const n = codes[i];
		if (n === 0) Object.assign(s, plain());
		else if (n === 1) s.bold = true;
		else if (n === 2) s.dim = true;
		else if (n === 3) s.italic = true;
		else if (n === 4) s.underline = true;
		else if (n === 22) s.bold = s.dim = false;
		else if (n === 23) s.italic = false;
		else if (n === 24) s.underline = false;
		else if (n >= 30 && n <= 37) s.fg = names[n - 30];
		else if (n === 39) s.fg = '';
		else if (n >= 40 && n <= 47) s.bg = names[n - 40];
		else if (n === 49) s.bg = '';
		else if (n >= 90 && n <= 97) s.fg = `bright-${names[n - 90]}`;
		else if (n >= 100 && n <= 107) s.bg = `bright-${names[n - 100]}`;
		// 256-color and true color: not styled, but skip their arguments.
		else if (n === 38 || n === 48) i += codes[i + 1] === 5 ? 2 : codes[i + 1] === 2 ? 4 : 1;
	}
}

/** Splits one line of output into styled segments. */
export function parseAnsi(text: string): AnsiSegment[] {
	if (!text.includes('\x1b')) return [{ text, cls: '' }];
	const out: AnsiSegment[] = [];
	const style = plain();
	let cls = '';
	let last = 0;
	const push = (t: string) => {
		if (!t) return;
		const prev = out[out.length - 1];
		if (prev && prev.cls === cls) prev.text += t;
		else out.push({ text: t, cls });
	};
	for (const m of text.matchAll(escape)) {
		push(text.slice(last, m.index));
		last = m.index + m[0].length;
		if (m[2] === 'm') {
			apply(style, m[1] ?? '');
			cls = classes(style);
		}
	}
	push(text.slice(last));
	return out;
}
