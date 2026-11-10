import type { JobTemplate, JobType, LaunchRequest, OverridableField } from './api/client.js';
import { parseArgv, parseVars } from './format.js';

/** The fields a job template can prompt for at launch, and the job types that have them. */
export const overridable: { field: OverridableField; label: string; types: JobType[] }[] = [
	{ field: 'inventory', label: 'Inventory', types: ['ansible', 'script', 'container'] },
	{ field: 'extraVars', label: 'Extra variables', types: ['ansible', 'script', 'container'] },
	{ field: 'limit', label: 'Limit', types: ['ansible', 'script', 'container'] },
	{ field: 'command', label: 'Command', types: ['container'] },
	{ field: 'tags', label: 'Tags', types: ['ansible'] },
	{ field: 'skipTags', label: 'Skip tags', types: ['ansible'] }
];

/** The fields a job type can prompt for. */
export function overridesFor(jobType: JobType) {
	return overridable.filter((o) => o.types.includes(jobType));
}

/** What a launcher entered for the prompted fields, as typed. */
export type LaunchValues = {
	inventoryId: string;
	extraVars: string;
	limit: string;
	command: string;
	tags: string;
	skipTags: string;
};

function equal(a: unknown, b: unknown): boolean {
	if (a === b) return true;
	if (typeof a !== 'object' || typeof b !== 'object' || a === null || b === null) return false;
	if (Array.isArray(a) !== Array.isArray(b)) return false;
	const ka = Object.keys(a);
	const kb = Object.keys(b);
	if (ka.length !== kb.length) return false;
	return ka.every(
		(k) =>
			Object.hasOwn(b, k) &&
			equal((a as Record<string, unknown>)[k], (b as Record<string, unknown>)[k])
	);
}

/**
 * The launch request for what a launcher entered (check the values first):
 * only prompted fields that differ from the template. Sending the
 * template's own values back would need the use role on its inventory, and
 * the API refuses some extra variables at launch that a template may have.
 * The API merges extra variables over the template's, so keys the launcher
 * removed keep the template's value; removedVarKeys lists them.
 */
export function buildLaunchRequest(
	t: JobTemplate,
	v: LaunchValues
): { body: LaunchRequest; removedVarKeys: string[] } {
	const prompted = (f: OverridableField) => t.allowOverrides.includes(f);
	const body: LaunchRequest = {};
	let removedVarKeys: string[] = [];

	if (prompted('inventory') && v.inventoryId && v.inventoryId !== (t.inventoryId ?? '')) {
		body.inventoryId = v.inventoryId;
	}
	if (prompted('limit') && v.limit.trim() !== t.limit) body.limit = v.limit.trim();
	if (prompted('tags') && v.tags.trim() !== t.tags) body.tags = v.tags.trim();
	if (prompted('skipTags') && v.skipTags.trim() !== t.skipTags) body.skipTags = v.skipTags.trim();
	if (prompted('command')) {
		const command = parseArgv(v.command);
		if (!equal(command, t.command)) body.command = command;
	}
	if (prompted('extraVars')) {
		const vars = parseVars(v.extraVars);
		const changed = Object.fromEntries(
			Object.entries(vars).filter(([k, val]) => !equal(val, t.extraVars[k]))
		);
		if (Object.keys(changed).length) body.extraVars = changed;
		removedVarKeys = Object.keys(t.extraVars).filter((k) => !Object.hasOwn(vars, k));
	}
	return { body, removedVarKeys };
}
