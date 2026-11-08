import { api, type ResourceType, type Role, type RoleGrant } from './api/client.js';
import { session } from './session.svelte.js';

/**
 * The signed-in user's role grants, directly and through teams. Pages use
 * them to offer only the actions the user can take; the API still checks
 * every request.
 */
export const myGrants = $state<{ items: RoleGrant[] }>({ items: [] });

/** Reloads the signed-in user's grants (e.g. after creating a resource, which grants admin on it). */
export async function loadMyGrants(): Promise<void> {
	const u = session.user;
	if (!u || u.isAdmin) {
		myGrants.items = [];
		return;
	}
	const { data } = await api.GET('/api/v1/role-grants', {
		params: { query: { userId: u.id, limit: 500 } }
	});
	myGrants.items = data?.items ?? [];
}

// The roles each role includes besides itself (as in internal/rbac).
const implies: Partial<Record<Role, Role[]>> = {
	admin: ['use', 'execute', 'read'],
	use: ['read'],
	execute: ['read']
};

/**
 * Whether the signed-in user holds want on a resource, or with id null, on
 * every resource of the type (admin on every resource allows creating them).
 */
export function can(type: ResourceType, id: string | null, want: Role): boolean {
	const u = session.user;
	if (!u) return false;
	if (u.isAdmin || (u.isAuditor && want === 'read')) return true;
	return myGrants.items.some(
		(g) =>
			g.resourceType === type &&
			(g.resourceId === null || g.resourceId === id) &&
			(g.role === want || Boolean(implies[g.role]?.includes(want)))
	);
}

/** Whether the signed-in user may create resources of a type. */
export function canCreate(type: ResourceType): boolean {
	return can(type, null, 'admin');
}
