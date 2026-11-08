import createClient, { type Middleware } from 'openapi-fetch';
import type { components, paths } from './schema.js';

export type User = components['schemas']['User'];
export type ApiToken = components['schemas']['ApiToken'];
export type CreatedApiToken = components['schemas']['CreatedApiToken'];
export type Problem = components['schemas']['Problem'];
export type Health = components['schemas']['Health'];
export type Job = components['schemas']['Job'];
export type JobStatus = components['schemas']['JobStatus'];
export type JobEvent = components['schemas']['JobEvent'];
export type JobTemplate = components['schemas']['JobTemplate'];
export type JobHostSummary = components['schemas']['JobHostSummary'];
export type TokenScope = components['schemas']['TokenScope'];
export type Team = components['schemas']['Team'];
export type TeamMember = components['schemas']['TeamMember'];
export type Role = components['schemas']['Role'];
export type ResourceType = components['schemas']['ResourceType'];
export type RoleGrant = components['schemas']['RoleGrant'];
export type RoleGrantCreate = components['schemas']['RoleGrantCreate'];
export type AuditEntry = components['schemas']['AuditEntry'];
export type AuditAction = components['schemas']['AuditAction'];
export type AuditResourceType = components['schemas']['AuditResourceType'];
export type Schedule = components['schemas']['Schedule'];
export type NotificationTarget = components['schemas']['NotificationTarget'];
export type NotificationKind = components['schemas']['NotificationKind'];
export type NotificationEvent = components['schemas']['NotificationEvent'];
export type TemplateNotification = components['schemas']['TemplateNotification'];
export type NotificationDelivery = components['schemas']['NotificationDelivery'];

/** Notification events, with labels. */
export const notificationEvents: { event: NotificationEvent; label: string }[] = [
	{ event: 'started', label: 'Started' },
	{ event: 'succeeded', label: 'Succeeded' },
	{ event: 'failed', label: 'Failed' },
	{ event: 'canceled', label: 'Canceled' }
];

/** Resource types roles can be granted on, with display names. */
export const resourceTypes: { type: ResourceType; label: string; plural: string }[] = [
	{ type: 'inventory', label: 'Inventory', plural: 'inventories' },
	{ type: 'credential', label: 'Credential', plural: 'credentials' },
	{ type: 'project', label: 'Project', plural: 'projects' },
	{ type: 'job_template', label: 'Job template', plural: 'job templates' }
];

/** The roles that can be granted on each resource type. */
export const rolesFor: Record<ResourceType, Role[]> = {
	inventory: ['admin', 'use', 'read'],
	credential: ['admin', 'use', 'read'],
	project: ['admin', 'use', 'read'],
	job_template: ['admin', 'execute', 'read']
};

/** Roles granted without a resource. */
export const globalRoles: Role[] = ['system_admin', 'auditor'];

/** Lists resources of a type (up to 500), for pickers. */
export async function listResources(type: ResourceType): Promise<{ id: string; name: string }[]> {
	const query = { params: { query: { limit: 500 } } };
	const res =
		type === 'inventory'
			? await api.GET('/api/v1/inventories', query)
			: type === 'credential'
				? await api.GET('/api/v1/credentials', query)
				: type === 'project'
					? await api.GET('/api/v1/projects', query)
					: await api.GET('/api/v1/job-templates', query);
	return res.data?.items ?? [];
}

/** Event types for a host's task result in an Ansible job. */
export const hostEventTypes = ['host_ok', 'host_failed', 'host_skipped', 'host_unreachable'];

/** Statuses after which a job no longer changes. */
export const finishedStatuses: readonly JobStatus[] = ['successful', 'failed', 'error', 'canceled'];

/** Typed client for the Dispatch Ops API, generated from api/openapi.yaml. */
export const api = createClient<paths>({ baseUrl: '', credentials: 'same-origin' });

const unauthorizedListeners = new Set<() => void>();

/** Calls fn whenever an authenticated API call comes back 401 (e.g. the session expired). */
export function onUnauthorized(fn: () => void): () => void {
	unauthorizedListeners.add(fn);
	return () => unauthorizedListeners.delete(fn);
}

const sessionExpiry: Middleware = {
	onResponse({ request, response }) {
		if (response.status === 401 && !request.url.endsWith('/api/v1/auth/login')) {
			for (const fn of unauthorizedListeners) fn();
		}
	}
};
api.use(sessionExpiry);

/** A readable message for an API error response. */
export function errorMessage(error: unknown): string {
	if (error && typeof error === 'object' && 'title' in error) {
		const p = error as Problem;
		return p.detail ? `${p.title}: ${p.detail}` : p.title;
	}
	if (error instanceof Error) return error.message;
	return 'Something went wrong';
}
