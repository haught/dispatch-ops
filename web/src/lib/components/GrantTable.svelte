<script lang="ts">
	import { resourceTypes, type RoleGrant } from '#lib/api/client.js';
	import { fmtTime } from '#lib/format.js';

	let {
		grants,
		onrevoke,
		showHolder = true
	}: {
		grants: RoleGrant[];
		/** Shows a Revoke button on each row when set. */
		onrevoke?: (g: RoleGrant) => void;
		showHolder?: boolean;
	} = $props();

	function resource(g: RoleGrant): string {
		if (!g.resourceType) return 'Global';
		const rt = resourceTypes.find((r) => r.type === g.resourceType);
		if (!g.resourceId) return `All ${rt?.plural ?? g.resourceType}`;
		return `${rt?.label ?? g.resourceType}: ${g.resourceName || g.resourceId}`;
	}
</script>

<div class="table-wrap">
	<table>
		<thead>
			<tr>
				<th>Role</th>
				<th>On</th>
				{#if showHolder}<th>Held by</th>{/if}
				<th>Granted</th>
				{#if onrevoke}<th></th>{/if}
			</tr>
		</thead>
		<tbody>
			{#each grants as g (g.id)}
				<tr>
					<td><span class="badge" class:accent={g.role === 'system_admin'}>{g.role}</span></td>
					<td>{resource(g)}</td>
					{#if showHolder}
						<td>
							{#if g.user}
								<a href="/users/{g.user.id}">{g.user.username}</a>
							{:else if g.team}
								<a href="/teams/{g.team.id}">{g.team.name}</a>
								<span class="muted small">(team)</span>
							{/if}
						</td>
					{/if}
					<td>{fmtTime(g.createdAt)}</td>
					{#if onrevoke}
						<td class="row-actions">
							<button class="danger" onclick={() => onrevoke(g)}>Revoke</button>
						</td>
					{/if}
				</tr>
			{/each}
		</tbody>
	</table>
</div>
