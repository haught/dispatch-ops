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
