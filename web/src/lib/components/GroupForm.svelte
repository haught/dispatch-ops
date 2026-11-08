<script lang="ts">
	import { untrack } from 'svelte';
	import { api, errorMessage, type Group } from '#lib/api/client.js';
	import { fmtVars, parseVars, varsError } from '#lib/format.js';
	import VarsField from './VarsField.svelte';

	/** Adds a host group to an inventory, or edits (or with readonly, shows) one. Key it on the group. */
	let {
		inventoryId,
		group = null,
		groups,
		readonly = false,
		onsaved,
		oncancel
	}: {
		inventoryId: string;
		group?: Group | null;
		/** Every group in the inventory, to choose children from. */
		groups: Group[];
		readonly?: boolean;
		onsaved: () => void;
		oncancel: () => void;
	} = $props();

	let name = $state(untrack(() => group?.name ?? ''));
	let description = $state(untrack(() => group?.description ?? ''));
	let childIds = $state<string[]>(untrack(() => [...(group?.childIds ?? [])]));
	let vars = $state(untrack(() => fmtVars(group?.variables)));
	let saving = $state(false);
	let error = $state('');

	const others = $derived(groups.filter((g) => g.id !== group?.id));

	async function save(event: SubmitEvent) {
		event.preventDefault();
		error = varsError(vars);
		if (error) return;
		saving = true;
		const body = { name: name.trim(), description, childIds, variables: parseVars(vars) };
		const { error: err } = group
			? await api.PATCH('/api/v1/groups/{groupId}', {
					params: { path: { groupId: group.id } },
					body
				})
			: await api.POST('/api/v1/inventories/{inventoryId}/groups', {
					params: { path: { inventoryId } },
					body
				});
		saving = false;
		if (err) error = errorMessage(err);
		else onsaved();
	}
</script>

<form onsubmit={save} id="group-form">
	<h3>{readonly ? group?.name : group ? `Edit ${group.name}` : 'New group'}</h3>
	{#if error}<p class="error" role="alert">{error}</p>{/if}
	<fieldset disabled={readonly}>
		<div class="form-grid">
			<label>
				Name
				<input
					bind:value={name}
					required
					maxlength="255"
					pattern="[A-Za-z_][A-Za-z0-9_]*"
					title="Letters, digits and underscores, not starting with a digit"
					placeholder="webservers"
				/>
			</label>
			<label>Description <input bind:value={description} maxlength="4096" /></label>
		</div>
		{#if others.length}
			<div class="field">
				Child groups
				<div class="checks">
					{#each others as g (g.id)}
						<label><input type="checkbox" bind:group={childIds} value={g.id} /> {g.name}</label>
					{/each}
				</div>
			</div>
		{/if}
		<VarsField bind:value={vars} label="Group variables" />
		{#if !readonly}
			<p class="muted small">Add hosts to the group from each host's form.</p>
		{/if}
	</fieldset>
	<div class="form-actions">
		{#if !readonly}
			<button type="submit" disabled={saving}>{group ? 'Save group' : 'Add group'}</button>
		{/if}
		<button type="button" class="secondary" onclick={oncancel}
			>{readonly ? 'Close' : 'Cancel'}</button
		>
	</div>
</form>

<style>
	form {
		margin-top: 1.25rem;
		padding-top: 1rem;
		border-top: 1px solid var(--border);
	}

	h3 {
		font-size: 1rem;
		margin: 0 0 0.75rem;
	}

	fieldset {
		border: none;
		padding: 0;
		margin: 0;
	}

	.field {
		font-weight: 500;
		margin-bottom: 0.75rem;
	}
</style>
