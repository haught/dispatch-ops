<script lang="ts">
	import {
		api,
		errorMessage,
		type NotificationKind,
		type NotificationTarget
	} from '#lib/api/client.js';
	import { session } from '#lib/session.svelte.js';

	let targets = $state<NotificationTarget[]>([]);
	let loading = $state(true);
	let error = $state('');
	// Result of the last test, per target.
	let tested = $state<Record<string, { ok: boolean; error: string }>>({});

	const isAdmin = $derived(Boolean(session.user?.isAdmin));

	// The form creates a target, or edits the one in editing.
	let editing = $state<NotificationTarget | null>(null);
	let name = $state('');
	let description = $state('');
	let kind = $state<NotificationKind>('webhook');
	let url = $state('');
	let authorization = $state('');
	let signingSecret = $state('');
	let slackUrl = $state('');
	let googleChatUrl = $state('');
	let host = $state('');
	let port = $state('');
	let security = $state('starttls');
	let username = $state('');
	let password = $state('');
	let from = $state('');
	let to = $state('');
	let formError = $state('');

	async function load() {
		const { data, error: err } = await api.GET('/api/v1/notification-targets', {
			params: { query: { limit: 500 } }
		});
		loading = false;
		if (data) targets = data.items;
		else error = errorMessage(err);
	}

	$effect(() => {
		load();
	});

	function reset() {
		editing = null;
		name = description = url = authorization = signingSecret = slackUrl = googleChatUrl = '';
		host = port = username = password = from = to = '';
		security = 'starttls';
		kind = 'webhook';
		formError = '';
	}

	function edit(t: NotificationTarget) {
		reset();
		editing = t;
		name = t.name;
		description = t.description;
		kind = t.kind;
		const c = t.config as Record<string, unknown>;
		url = String(c.url ?? '');
		host = String(c.host ?? '');
		port = c.port ? String(c.port) : '';
		security = String(c.security ?? 'starttls');
		username = String(c.username ?? '');
		from = String(c.from ?? '');
		to = Array.isArray(c.to) ? c.to.join(', ') : '';
	}

	function config(): Record<string, unknown> {
		switch (kind) {
			case 'webhook':
				return { url };
			case 'email':
				return {
					host,
					port: port ? Number(port) : undefined,
					security,
					username: username || undefined,
					from,
					to: to
						.split(',')
						.map((s) => s.trim())
						.filter(Boolean)
				};
			default:
				return {};
		}
	}

	/** The secrets typed in; blank fields keep (when editing) or omit a secret. */
	function secrets(): Record<string, string> {
		const s: Record<string, string> = {};
		const set = (k: string, v: string) => {
			if (v) s[k] = v;
		};
		if (kind === 'webhook') {
			set('authorization', authorization);
			set('signingSecret', signingSecret);
		} else if (kind === 'slack') {
			set('webhookUrl', slackUrl);
		} else if (kind === 'googlechat') {
			set('webhookUrl', googleChatUrl);
		} else {
			set('password', password);
		}
		return s;
	}

	async function save(event: SubmitEvent) {
		event.preventDefault();
		formError = '';
		const { error: err } = editing
			? await api.PATCH('/api/v1/notification-targets/{targetId}', {
					params: { path: { targetId: editing.id } },
					body: { name, description, config: config(), secrets: secrets() }
				})
			: await api.POST('/api/v1/notification-targets', {
					body: { name, description, kind, config: config(), secrets: secrets() }
				});
		if (err) {
			formError = errorMessage(err);
			return;
		}
		reset();
		await load();
	}

	async function test(t: NotificationTarget) {
		const { data, error: err } = await api.POST('/api/v1/notification-targets/{targetId}/test', {
			params: { path: { targetId: t.id } }
		});
		tested[t.id] = data ?? { ok: false, error: errorMessage(err) };
	}

	async function remove(t: NotificationTarget) {
		const { error: err } = await api.DELETE('/api/v1/notification-targets/{targetId}', {
			params: { path: { targetId: t.id } }
		});
		if (err) error = errorMessage(err);
		if (editing?.id === t.id) reset();
		await load();
	}

	function summary(t: NotificationTarget): string {
		const c = t.config as Record<string, unknown>;
		if (t.kind === 'webhook') return String(c.url ?? '');
		if (t.kind === 'email' && Array.isArray(c.to)) return `to ${c.to.join(', ')} via ${c.host}`;
		return '';
	}

	const secretHint = $derived(editing ? 'Leave blank to keep the current value' : '');
</script>

<h1>Notifications</h1>
<p class="muted">
	Targets receive notifications about jobs. Choose which targets a job template notifies, and on
	which events, on the template's page.
</p>

{#if error}
	<p class="error" role="alert">{error}</p>
{/if}

<section class="card">
	{#if loading}
		<p class="muted">Loading…</p>
	{:else if targets.length === 0}
		<p class="muted">No notification targets yet.</p>
	{:else}
		<div class="table-wrap">
			<table>
				<thead>
					<tr>
						<th>Name</th>
						<th>Kind</th>
						<th>Sends to</th>
						<th></th>
					</tr>
				</thead>
				<tbody>
					{#each targets as t (t.id)}
						<tr>
							<td>
								{t.name}
								{#if t.description}<div class="muted small">{t.description}</div>{/if}
							</td>
							<td>{t.kind}</td>
							<td class="small">
								<code>{summary(t)}</code>
								{#if tested[t.id]}
									<div class:error={!tested[t.id].ok} class:ok={tested[t.id].ok}>
										{tested[t.id].ok ? 'Test sent.' : tested[t.id].error}
									</div>
								{/if}
							</td>
							<td>
								{#if isAdmin}
									<div class="row-actions">
										<button class="secondary" onclick={() => test(t)}>Test</button>
										<button class="secondary" onclick={() => edit(t)}>Edit</button>
										<button class="danger" onclick={() => remove(t)}>Delete</button>
									</div>
								{/if}
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>

{#if isAdmin}
	<section class="card">
		<h2>{editing ? `Edit ${editing.name}` : 'New notification target'}</h2>
		{#if formError}<p class="error" role="alert">{formError}</p>{/if}
		<form onsubmit={save}>
			<div class="form-grid">
				<label>Name <input bind:value={name} maxlength="255" required /></label>
				<label>Description <input bind:value={description} /></label>
				<label>
					Kind
					<select bind:value={kind} disabled={editing !== null}>
						<option value="webhook">Webhook</option>
						<option value="slack">Slack</option>
						<option value="googlechat">Google Chat</option>
						<option value="email">Email</option>
					</select>
				</label>
			</div>

			{#if kind === 'webhook'}
				<div class="form-grid">
					<label>URL <input bind:value={url} type="url" required placeholder="https://…" /></label>
					<label>
						Authorization header
						<input
							bind:value={authorization}
							type="password"
							placeholder={secretHint || 'Optional, e.g. Bearer …'}
						/>
					</label>
					<label>
						Signing secret
						<input
							bind:value={signingSecret}
							type="password"
							placeholder={secretHint || 'Optional'}
						/>
					</label>
				</div>
				<p class="small muted">
					Receives a POST with the event as JSON. With a signing secret, <code
						>X-Dispatch-Signature</code
					>
					is
					<code>sha256=</code> the HMAC-SHA256 of
					<code>&lt;X-Dispatch-Timestamp&gt;.&lt;body&gt;</code>.
				</p>
			{:else if kind === 'slack'}
				<label>
					Incoming webhook URL
					<input
						bind:value={slackUrl}
						type="password"
						required={!editing}
						placeholder={secretHint || 'https://hooks.slack.com/services/…'}
					/>
				</label>
			{:else if kind === 'googlechat'}
				<label>
					Incoming webhook URL
					<input
						bind:value={googleChatUrl}
						type="password"
						required={!editing}
						placeholder={secretHint ||
							'https://chat.googleapis.com/v1/spaces/…/messages?key=…&token=…'}
					/>
				</label>
				<p class="small muted">
					In the space, open <em>Apps &amp; integrations → Webhooks</em> and add a webhook. Each job's
					notifications are posted as cards in one thread.
				</p>
			{:else}
				<div class="form-grid">
					<label>SMTP host <input bind:value={host} required /></label>
					<label>Port <input bind:value={port} inputmode="numeric" placeholder="587" /></label>
					<label>
						Security
						<select bind:value={security}>
							<option value="starttls">STARTTLS</option>
							<option value="tls">TLS</option>
							<option value="none">None</option>
						</select>
					</label>
					<label>Username <input bind:value={username} autocomplete="off" /></label>
					<label>
						Password
						<input
							bind:value={password}
							type="password"
							autocomplete="new-password"
							placeholder={secretHint}
						/>
					</label>
					<label
						>From <input
							bind:value={from}
							required
							placeholder="Dispatch Ops <ops@example.com>"
						/></label
					>
				</div>
				<label>To <input bind:value={to} required placeholder="Comma-separated addresses" /></label>
			{/if}

			<div class="actions">
				<button type="submit">{editing ? 'Save' : 'Create target'}</button>
				{#if editing}<button type="button" class="secondary" onclick={reset}>Cancel</button>{/if}
			</div>
		</form>
	</section>
{/if}

<style>
	.actions {
		display: flex;
		gap: 0.5rem;
	}

	.ok {
		color: var(--ok);
	}
</style>
