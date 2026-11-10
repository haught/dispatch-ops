import { api, type JobTemplate } from './api/client.js';

/** A resource a job template uses; name is null where the caller can't see it. */
export type Ref = { id: string; name: string | null };

export type TemplateRefs = {
	ee: Ref | null;
	project: Ref | null;
	inventory: Ref | null;
	credentials: Ref[];
};

export const noRefs: TemplateRefs = { ee: null, project: null, inventory: null, credentials: [] };

/** Looks up an inventory's name; null where the caller can't see it. */
export async function inventoryRef(id: string): Promise<Ref> {
	const r = await api.GET('/api/v1/inventories/{inventoryId}', {
		params: { path: { inventoryId: id } }
	});
	return { id, name: r.data?.name ?? null };
}

/** Looks up the names of what a job template uses. */
export async function loadTemplateRefs(t: JobTemplate): Promise<TemplateRefs> {
	const ref = (id: string, data: { name: string } | undefined): Ref => ({
		id,
		name: data?.name ?? null
	});
	const [ee, project, inventory, ...credentials] = await Promise.all([
		api
			.GET('/api/v1/execution-environments/{executionEnvironmentId}', {
				params: { path: { executionEnvironmentId: t.executionEnvironmentId } }
			})
			.then((r) => ref(t.executionEnvironmentId, r.data)),
		t.projectId
			? api
					.GET('/api/v1/projects/{projectId}', { params: { path: { projectId: t.projectId } } })
					.then((r) => ref(t.projectId!, r.data))
			: null,
		t.inventoryId ? inventoryRef(t.inventoryId) : null,
		...t.credentialIds.map((cid) =>
			api
				.GET('/api/v1/credentials/{credentialId}', { params: { path: { credentialId: cid } } })
				.then((r) => ref(cid, r.data))
		)
	]);
	return { ee, project, inventory, credentials: credentials.filter((c) => c !== null) };
}
