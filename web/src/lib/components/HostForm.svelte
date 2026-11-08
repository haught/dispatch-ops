<script lang="ts">
	import { untrack } from 'svelte';
	import { api, errorMessage, type Group, type Host } from '#lib/api/client.js';
	import { fmtVars, parseVars, varsError } from '#lib/format.js';
	import VarsField from './VarsField.svelte';

	/** Adds a host to an inventory, or edits (or with readonly, shows) one. Key it on the host. */
	let {
		inventoryId,
		host = null,
		groups,
		readonly = false,
		onsaved,
		oncancel
	}: {
		inventoryId: string;
		host?: Host | null;
		groups: Group[];
		readonly?: boolean;
		onsaved: () => void;
		oncancel: () => void;
	} = $props();

	let name = $state(untrack(() => host?.name ?? ''));
	let description = $state(untrack(() => host?.description ?? ''));
	let enabled = $state(untrack(() => host?.enabled ?? true));
	let groupIds = $state<string[]>(untrack(() => [...(host?.groupIds ?? [])]));
	let vars = $state(untrack(() => fmtVars(host?.variables)));
	let saving = $state(false);
	let error = $state('');

	async function save(event: SubmitEvent) {
		event.preventDefault();
		error = varsError(vars);
		if (error) return;
		saving = true;
		const body = { name: name.trim(), description, enabled, groupIds, variables: parseVars(vars) };
		const { error: err } = host
			? await api.PATCH('/api/v1/hosts/{hostId}', { params: { path: { hostId: host.id } }, body })
			: await api.POST('/api/v1/inventories/{inventoryId}/hosts', {
					params: { path: { inventoryId } },
					body
				});
		saving = false;
		if (err) error = errorMessage(err);
		else onsaved();
	}
</script>

<form onsubmit={save} id="host-form">
	<h3>{readonly ? host?.name : host ? `Edit ${host.name}` : 'New host'}</h3>
	{#if error}<p class="error" role="alert">{error}</p>{/if}
	<fieldset disabled={readonly}>
		<div class="form-grid">
			<label>
				Name or address
				<input
					bind:value={name}
					required
					maxlength="255"
					pattern="\S+"
					title="No spaces"
					placeholder="web1.example.com"
				/>
			</label>
			<label>Description <input bind:value={description} maxlength="4096" /></label>
			<label class="check">
				<input type="checkbox" bind:checked={enabled} /> Enabled
				<span class="muted small">Disabled hosts are left out of jobs</span>
			</label>
		</div>
		{#if groups.length}
			<div class="field">
				Groups
				<div class="checks">
					{#each groups as g (g.id)}
						<label><input type="checkbox" bind:group={groupIds} value={g.id} /> {g.name}</label>
					{/each}
				</div>
			</div>
		{/if}
		<VarsField bind:value={vars} label="Host variables" />
	</fieldset>
	<div class="form-actions">
		{#if !readonly}
			<button type="submit" disabled={saving}>{host ? 'Save host' : 'Add host'}</button>
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

	.check {
		display: flex;
		align-items: center;
		flex-wrap: wrap;
		gap: 0.4rem;
		padding-bottom: 0.5rem;
	}

	.field {
		font-weight: 500;
		margin-bottom: 0.75rem;
	}
</style>
