<script lang="ts">
	import { untrack } from 'svelte';
	import {
		api,
		errorMessage,
		globalRoles,
		listResources,
		resourceTypes,
		rolesFor,
		type ResourceType,
		type Role,
		type RoleGrantCreate
	} from '#lib/api/client.js';

	let {
		userId,
		teamId,
		resourceType,
		resourceId,
		ongranted
	}: {
		/** Grant to this user, instead of choosing a holder. */
		userId?: string;
		/** Grant to this team, instead of choosing a holder. */
		teamId?: string;
		/** With resourceId, grant on this resource instead of choosing one. */
		resourceType?: ResourceType;
		resourceId?: string;
		ongranted: () => void;
	} = $props();

	const fixedHolder = $derived(Boolean(userId || teamId));
	const fixedResource = $derived(Boolean(resourceType && resourceId));

	let holderKind = $state<'user' | 'team'>('user');
	let holderId = $state('');
	let holders = $state<{ id: string; name: string }[]>([]);

	let scope = $state<ResourceType | 'global'>(untrack(() => resourceType) ?? 'job_template');
	let chosenResourceId = $state('');
	let resources = $state<{ id: string; name: string }[]>([]);
	let role = $state<Role>(untrack(() => (resourceType ? rolesFor[resourceType][1] : 'execute')));

	let busy = $state(false);
	let error = $state('');

	const roles = $derived(scope === 'global' ? globalRoles : rolesFor[scope]);

	// Load who can hold the role.
	$effect(() => {
		if (fixedHolder) return;
		const kind = holderKind;
		holderId = '';
		(async () => {
			if (kind === 'user') {
				const { data } = await api.GET('/api/v1/users', { params: { query: { limit: 500 } } });
				holders = (data?.items ?? []).map((u) => ({
					id: u.id,
					name: u.displayName ? `${u.username} (${u.displayName})` : u.username
				}));
			} else {
				const { data } = await api.GET('/api/v1/teams', { params: { query: { limit: 500 } } });
				holders = data?.items ?? [];
			}
		})();
	});

	// Load the resources of the chosen type.
	$effect(() => {
		if (fixedResource) return;
		const s = scope;
		chosenResourceId = '';
		resources = [];
		if (s !== 'global') listResources(s).then((r) => (resources = r));
	});

	// Keep the role valid for the chosen type.
	function scopeChanged() {
		if (!roles.includes(role)) role = roles[roles.length - 1];
	}

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		busy = true;
		error = '';
		const body: RoleGrantCreate = { role };
		if (userId) body.userId = userId;
		else if (teamId) body.teamId = teamId;
		else if (holderKind === 'user') body.userId = holderId;
		else body.teamId = holderId;
		if (fixedResource) {
			body.resourceType = resourceType;
			body.resourceId = resourceId;
		} else if (scope !== 'global') {
			body.resourceType = scope;
			if (chosenResourceId) body.resourceId = chosenResourceId;
		}
		const { error: err } = await api.POST('/api/v1/role-grants', { body });
		busy = false;
		if (err) {
			error = errorMessage(err);
			return;
		}
		ongranted();
	}
</script>

<form onsubmit={submit}>
	<div class="form-grid">
		{#if !fixedHolder}
			<label>
				Grant to
				<select bind:value={holderKind}>
					<option value="user">User</option>
					<option value="team">Team</option>
				</select>
			</label>
			<label>
				{holderKind === 'user' ? 'User' : 'Team'}
				<select bind:value={holderId} required>
					<option value="" disabled>Choose…</option>
					{#each holders as h (h.id)}
						<option value={h.id}>{h.name}</option>
					{/each}
				</select>
			</label>
		{/if}
		{#if !fixedResource}
			<label>
				On
				<select bind:value={scope} onchange={scopeChanged}>
					{#each resourceTypes as rt (rt.type)}
						<option value={rt.type}>{rt.label}</option>
					{/each}
					<option value="global">Everything (global role)</option>
				</select>
			</label>
			{#if scope !== 'global'}
				<label>
					Which
					<select bind:value={chosenResourceId}>
						<option value="">All {resourceTypes.find((r) => r.type === scope)?.plural}</option>
						{#each resources as r (r.id)}
							<option value={r.id}>{r.name}</option>
						{/each}
					</select>
				</label>
			{/if}
		{/if}
		<label>
			Role
			<select bind:value={role}>
				{#each roles as r (r)}
					<option value={r}>{r}</option>
				{/each}
			</select>
		</label>
		<label>
			<button type="submit" disabled={busy}>Grant</button>
		</label>
	</div>
	{#if error}
		<p class="error" role="alert">{error}</p>
	{/if}
	<p class="muted small">
		<strong>admin</strong> manages a resource and its roles; <strong>use</strong> lets job templates
		reference it; <strong>execute</strong> launches a template; <strong>read</strong> shows it.
		{#if !fixedResource}<strong>admin</strong> on all of a type also allows creating them.{/if}
	</p>
</form>
