<script lang="ts">
	import { untrack } from 'svelte';
	import type { Credential, CredentialKindInfo } from '#lib/api/client.js';

	/**
	 * The input fields of a credential kind. Editing (credential given), blank
	 * secrets keep their current value. Key the component on the kind and the
	 * credential so it starts over when either changes.
	 */
	let { kind, credential = null }: { kind: CredentialKindInfo; credential?: Credential | null } =
		$props();

	type Row = { name: string; value: string; existing: boolean; remove: boolean };

	// Field values, starting from the public values when editing.
	let values = $state<Record<string, string>>(
		untrack(() =>
			Object.fromEntries(kind.fields.map((f) => [f.name, credential?.publicInputs[f.name] ?? '']))
		)
	);
	// Set secrets marked for removal.
	let remove = $state<Record<string, boolean>>({});
	// Freeform kinds: one row per value.
	let rows = $state<Row[]>(
		untrack(() =>
			credential
				? credential.secretFields.map((name) => ({
						name,
						value: '',
						existing: true,
						remove: false
					}))
				: [{ name: '', value: '', existing: false, remove: false }]
		)
	);

	const editing = $derived(credential !== null);

	function isSet(name: string): boolean {
		return Boolean(credential?.secretFields.includes(name));
	}

	function addRow() {
		rows.push({ name: '', value: '', existing: false, remove: false });
	}

	/**
	 * The inputs to send. Creating, the values given. Editing, only what
	 * changed: public values that differ, secrets typed in, and '' for each
	 * value to remove.
	 */
	export function inputs(): Record<string, string> {
		const out: Record<string, string> = {};
		if (kind.freeform) {
			for (const r of rows) {
				const name = r.name.trim();
				if (!name) continue;
				if (r.existing && r.remove) out[name] = '';
				else if (r.value) out[name] = r.value;
			}
			return out;
		}
		for (const f of kind.fields) {
			const v = values[f.name] ?? '';
			if (!credential) {
				if (v) out[f.name] = v;
			} else if (!f.secret) {
				if (v !== (credential.publicInputs[f.name] ?? '')) out[f.name] = v;
			} else if (remove[f.name]) {
				out[f.name] = '';
			} else if (v) {
				out[f.name] = v;
			}
		}
		return out;
	}
</script>

{#if kind.freeform}
	<div class="freeform">
		<span class="head">Name</span>
		<span class="head">Value</span>
		<span></span>
		{#each rows as r, i (i)}
			<input
				bind:value={r.name}
				aria-label="Name"
				readonly={r.existing}
				required={!r.existing && Boolean(r.value)}
				pattern="[A-Za-z_][A-Za-z0-9_]*"
				title="Letters, digits and underscores, not starting with a digit"
				placeholder="API_KEY"
			/>
			<input
				bind:value={r.value}
				aria-label="Value"
				type="password"
				autocomplete="new-password"
				disabled={r.remove}
				placeholder={r.existing ? 'Leave blank to keep the current value' : ''}
			/>
			{#if r.existing}
				<label class="inline small"><input type="checkbox" bind:checked={r.remove} /> Remove</label>
			{:else}
				<button
					type="button"
					class="secondary"
					aria-label="Remove row"
					onclick={() => rows.splice(i, 1)}>✕</button
				>
			{/if}
		{/each}
	</div>
	<p>
		<button type="button" class="secondary" onclick={addRow}>Add a value</button>
		<span class="muted small">All values are secret. Jobs get each one by name.</span>
	</p>
{:else}
	<div class="form-grid fields">
		{#each kind.fields as f (f.name)}
			<div class:wide={f.multiline}>
				<label>
					{f.label}
					{#if f.secret && editing}
						<span class="muted small">{isSet(f.name) ? '· set' : '· not set'}</span>
					{:else if f.secret}
						<span class="muted small">· secret</span>
					{/if}
					{#if f.multiline}
						<textarea
							bind:value={values[f.name]}
							class="code"
							rows="6"
							spellcheck="false"
							required={f.required && !(editing && f.secret)}
							disabled={remove[f.name]}
							placeholder={f.secret && isSet(f.name) ? 'Leave blank to keep the current value' : ''}
						></textarea>
					{:else}
						<input
							bind:value={values[f.name]}
							type={f.secret ? 'password' : 'text'}
							autocomplete={f.secret ? 'new-password' : 'off'}
							required={f.required && !(editing && f.secret)}
							disabled={remove[f.name]}
							placeholder={f.secret && isSet(f.name) ? 'Leave blank to keep the current value' : ''}
						/>
					{/if}
				</label>
				{#if f.secret && !f.required && isSet(f.name)}
					<label class="inline small">
						<input type="checkbox" bind:checked={remove[f.name]} /> Remove the {f.label.toLowerCase()}
					</label>
				{/if}
			</div>
		{/each}
	</div>
{/if}

<style>
	.fields {
		grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
	}

	.wide {
		grid-column: 1 / -1;
	}

	.inline {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
		font-weight: normal;
		margin-top: -0.5rem;
	}

	.freeform {
		display: grid;
		grid-template-columns: minmax(8rem, 1fr) 2fr auto;
		gap: 0.5rem;
		align-items: center;
	}

	.freeform .inline {
		margin: 0;
	}

	.head {
		font-weight: 500;
	}
</style>
