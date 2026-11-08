import { api, type User } from './api/client.js';

/** The signed-in user, shared across the app. */
export const session = $state<{ user: User | null; loaded: boolean }>({
	user: null,
	loaded: false
});

/** Loads the current user from the API. Leaves user null when signed out. */
export async function loadSession(): Promise<void> {
	const { data } = await api.GET('/api/v1/me');
	session.user = data ?? null;
	session.loaded = true;
}

export async function signOut(): Promise<void> {
	await api.POST('/api/v1/auth/logout');
	session.user = null;
}
