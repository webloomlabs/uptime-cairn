<script lang="ts">
	import { untrack } from 'svelte';
	import { api, ApiError } from '$lib/api';
	import type { Settings } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Button from '$lib/components/Button.svelte';
	import Field from '$lib/components/Field.svelte';

	/**
	 * Instance settings.
	 *
	 * Two things about this screen are decided rather than incidental.
	 *
	 * **The SMTP password is write-only and stays that way.** The read path
	 * cannot return it — it is encrypted at rest and the response carries no
	 * field for it at all — so an empty box means *keep the stored one* and never
	 * *clear it*. That is the same rule the status page editor follows for its
	 * password, for the same reason: a form that round-trips its own GET must not
	 * be able to destroy a credential it was never shown.
	 *
	 * **Numbers are sent as numbers or not at all.** Every retention and limit
	 * field is optional server-side, and an empty box means "leave it". Sending
	 * an empty string would be a type error the server would answer with a 400
	 * naming a field the user did not touch.
	 */
	/**
	 * The form's own copy, always non-null.
	 *
	 * A nullable `settings` reads naturally and does not survive `bind:` — the
	 * narrowing an `{#if}` gives the template does not reach inside a two-way
	 * binding, so every field becomes a null assertion. A blank value that the
	 * load overwrites keeps the bindings honest, and `loaded` is what the
	 * template branches on.
	 */
	let settings = $state<Settings>(blank());
	let loaded = $state(false);
	let loading = $state(true);

	function blank(): Settings {
		return {
			general: {},
			appearance: {},
			retention: {},
			smtp: {
				host: null,
				port: null,
				username: null,
				encryption: 'starttls',
				from_address: null,
				from_name: null
			},
			monitoring: {},
			security: {},
			telemetry: { enabled: false }
		};
	}
	let error = $state<unknown>(null);
	let saving = $state(false);
	let saved = $state(false);
	let fields = $state<Record<string, string>>({});

	let smtpPassword = $state('');
	let trustedProxies = $state('');

	async function load() {
		loading = true;
		error = null;
		try {
			settings = await api.get<Settings>('/settings');
			trustedProxies = (settings.security.trusted_proxies ?? []).join(', ');
			loaded = true;
		} catch (caught) {
			error = caught;
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		untrack(() => void load());
	});

	/** Empty means "leave it", which is not the same as zero. */
	function num(value: unknown): number | undefined {
		if (value === '' || value === null || value === undefined) return undefined;
		const parsed = Number(value);
		return Number.isFinite(parsed) ? parsed : undefined;
	}

	function list(value: string): string[] {
		return value
			.split(',')
			.map((entry) => entry.trim())
			.filter(Boolean);
	}

	async function save(event: SubmitEvent) {
		event.preventDefault();
		saving = true;
		saved = false;
		error = null;
		fields = {};

		try {
			const body: Record<string, unknown> = {
				general: {
					instance_name: settings.general.instance_name ?? '',
					base_url: settings.general.base_url ?? '',
					timezone: settings.general.timezone ?? '',
					locale: settings.general.locale ?? ''
				},
				retention: {
					raw_days: num(settings.retention.raw_days),
					rollup_1m_days: num(settings.retention.rollup_1m_days),
					rollup_5m_days: num(settings.retention.rollup_5m_days),
					rollup_1h_days: num(settings.retention.rollup_1h_days),
					rollup_1d_days: num(settings.retention.rollup_1d_days),
					webhook_delivery_days: num(settings.retention.webhook_delivery_days)
				},
				monitoring: {
					default_interval_seconds: num(settings.monitoring.default_interval_seconds),
					default_timeout_seconds: num(settings.monitoring.default_timeout_seconds),
					default_retries: num(settings.monitoring.default_retries),
					max_concurrent_checks: num(settings.monitoring.max_concurrent_checks)
				},
				security: {
					session_timeout_minutes: num(settings.security.session_timeout_minutes),
					login_rate_limit_per_minute: num(settings.security.login_rate_limit_per_minute),
					api_rate_limit_per_minute: num(settings.security.api_rate_limit_per_minute),
					require_totp: settings.security.require_totp ?? false,
					trusted_proxies: list(trustedProxies)
				},
				telemetry: { enabled: settings.telemetry.enabled }
			};

			const smtp: Record<string, unknown> = {
				host: settings.smtp.host ?? '',
				port: num(settings.smtp.port),
				username: settings.smtp.username ?? '',
				encryption: settings.smtp.encryption,
				from_address: settings.smtp.from_address ?? '',
				from_name: settings.smtp.from_name ?? ''
			};
			// Omitted entirely when the box is empty. An explicit null would be a
			// request to clear it, which is what somebody saving an unrelated
			// change must not accidentally do.
			if (smtpPassword) smtp.password = smtpPassword;
			body.smtp = smtp;

			settings = await api.patch<Settings>('/settings', body);
			trustedProxies = (settings.security.trusted_proxies ?? []).join(', ');
			smtpPassword = '';
			saved = true;
		} catch (caught) {
			error = caught;
			if (caught instanceof ApiError) fields = caught.fields();
		} finally {
			saving = false;
		}
	}

	const writable = $derived(session.allows('settings:write'));
	const smtpConfigured = $derived(Boolean(settings.smtp.host && settings.smtp.from_address));
</script>

<PageTitle title={t('settings.title')} />

{#if loading}
	<Spinner />
{:else if !loaded}
	<ErrorBox {error} onretry={load} />
{:else}
	<form onsubmit={save} class="max-w-2xl space-y-6">
		{#if error}
			<ErrorBox {error} />
		{/if}
		{#if saved}
			<p class="card px-4 py-3 text-sm" style="color: var(--color-up)">{t('settings.saved')}</p>
		{/if}

		<section class="card space-y-5 p-5">
			<h2 class="font-semibold">
				{t('settings.general')}<span style="color: var(--color-up)" aria-hidden="true">.</span>
			</h2>

			<Field
				label={t('settings.instanceName')}
				id="instance-name"
				error={fields['/general/instance_name']}
				hint={t('settings.instanceNameHint')}
			>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						class="field"
						bind:value={settings.general.instance_name}
						disabled={!writable}
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>

			<Field
				label={t('settings.baseUrl')}
				id="base-url"
				error={fields['/general/base_url']}
				hint={t('settings.baseUrlHint')}
			>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						class="field"
						type="url"
						placeholder="https://status.example.com"
						bind:value={settings.general.base_url}
						disabled={!writable}
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>

			<div class="grid gap-4 sm:grid-cols-2">
				<Field label={t('settings.timezone')} id="timezone" error={fields['/general/timezone']}>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							class="field"
							placeholder="Australia/Sydney"
							bind:value={settings.general.timezone}
							disabled={!writable}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
				<Field label={t('settings.locale')} id="locale" error={fields['/general/locale']}>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							class="field"
							placeholder="en"
							bind:value={settings.general.locale}
							disabled={!writable}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
			</div>
		</section>

		<section class="card space-y-5 p-5">
			<div>
				<h2 class="font-semibold">
					{t('settings.smtp')}<span style="color: var(--color-up)" aria-hidden="true">.</span>
				</h2>
				<p class="muted mt-1 text-sm">{t('settings.smtpHint')}</p>
			</div>

			{#if !smtpConfigured}
				<p class="text-sm" style="color: var(--color-pending)">{t('settings.smtpNotConfigured')}</p>
			{/if}

			<div class="grid gap-4 sm:grid-cols-[1fr_7rem]">
				<Field label={t('settings.smtpHost')} id="smtp-host" error={fields['/smtp/host']}>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							class="field"
							bind:value={settings.smtp.host}
							disabled={!writable}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
				<Field label={t('settings.smtpPort')} id="smtp-port" error={fields['/smtp/port']}>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							class="field"
							type="number"
							min="1"
							max="65535"
							bind:value={settings.smtp.port}
							disabled={!writable}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
			</div>

			<div class="grid gap-4 sm:grid-cols-2">
				<Field label={t('settings.smtpUsername')} id="smtp-user" error={fields['/smtp/username']}>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							class="field"
							autocomplete="off"
							bind:value={settings.smtp.username}
							disabled={!writable}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
				<Field
					label={t('settings.smtpPassword')}
					id="smtp-pass"
					error={fields['/smtp/password']}
					hint={t('settings.smtpPasswordHint')}
				>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							class="field"
							type="password"
							autocomplete="new-password"
							bind:value={smtpPassword}
							disabled={!writable}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
			</div>

			<div class="grid gap-4 sm:grid-cols-3">
				<Field
					label={t('settings.smtpEncryption')}
					id="smtp-enc"
					error={fields['/smtp/encryption']}
				>
					{#snippet children({ id, describedBy, invalid })}
						<select
							{id}
							class="field"
							bind:value={settings.smtp.encryption}
							disabled={!writable}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						>
							{#each ['none', 'starttls', 'tls'] as mode (mode)}
								<option value={mode}>{mode}</option>
							{/each}
						</select>
					{/snippet}
				</Field>
				<Field label={t('settings.smtpFrom')} id="smtp-from" error={fields['/smtp/from_address']}>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							class="field"
							type="email"
							bind:value={settings.smtp.from_address}
							disabled={!writable}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
				<Field
					label={t('settings.smtpFromName')}
					id="smtp-from-name"
					error={fields['/smtp/from_name']}
				>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							class="field"
							bind:value={settings.smtp.from_name}
							disabled={!writable}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
			</div>
		</section>

		<section class="card space-y-5 p-5">
			<div>
				<h2 class="font-semibold">
					{t('settings.retention')}<span style="color: var(--color-up)" aria-hidden="true">.</span>
				</h2>
				<p class="muted mt-1 text-sm">{t('settings.retentionHint')}</p>
			</div>

			<div class="grid gap-4 sm:grid-cols-3">
				{#each [{ key: 'raw_days', label: 'settings.rawDays' }, { key: 'rollup_1m_days', label: 'settings.rollup1m' }, { key: 'rollup_5m_days', label: 'settings.rollup5m' }, { key: 'rollup_1h_days', label: 'settings.rollup1h' }, { key: 'rollup_1d_days', label: 'settings.rollup1d' }, { key: 'webhook_delivery_days', label: 'settings.webhookDeliveryDays' }] as row (row.key)}
					<Field
						label={t(row.label)}
						id={`retention-${row.key}`}
						error={fields[`/retention/${row.key}`]}
					>
						{#snippet children({ id, describedBy, invalid })}
							<input
								{id}
								class="field"
								type="number"
								min="0"
								disabled={!writable}
								aria-describedby={describedBy}
								aria-invalid={invalid}
								value={(settings.retention as Record<string, number | undefined>)[row.key] ?? ''}
								oninput={(event) =>
									((settings.retention as Record<string, number | undefined>)[row.key] = num(
										event.currentTarget.value
									))}
							/>
						{/snippet}
					</Field>
				{/each}
			</div>
		</section>

		<section class="card space-y-5 p-5">
			<div>
				<h2 class="font-semibold">
					{t('settings.monitoring')}<span style="color: var(--color-up)" aria-hidden="true">.</span>
				</h2>
				<p class="muted mt-1 text-sm">{t('settings.monitoringHint')}</p>
			</div>

			<div class="grid gap-4 sm:grid-cols-2">
				<Field
					label={t('settings.defaultInterval')}
					id="default-interval"
					error={fields['/monitoring/default_interval_seconds']}
				>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							class="field"
							type="number"
							min="20"
							bind:value={settings.monitoring.default_interval_seconds}
							disabled={!writable}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
				<Field
					label={t('settings.defaultTimeout')}
					id="default-timeout"
					error={fields['/monitoring/default_timeout_seconds']}
				>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							class="field"
							type="number"
							min="1"
							bind:value={settings.monitoring.default_timeout_seconds}
							disabled={!writable}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
				<Field
					label={t('settings.defaultRetries')}
					id="default-retries"
					error={fields['/monitoring/default_retries']}
				>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							class="field"
							type="number"
							min="0"
							bind:value={settings.monitoring.default_retries}
							disabled={!writable}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
				<Field
					label={t('settings.maxConcurrent')}
					id="max-concurrent"
					error={fields['/monitoring/max_concurrent_checks']}
				>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							class="field"
							type="number"
							min="1"
							bind:value={settings.monitoring.max_concurrent_checks}
							disabled={!writable}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
			</div>
		</section>

		<section class="card space-y-5 p-5">
			<h2 class="font-semibold">
				{t('settings.security')}<span style="color: var(--color-up)" aria-hidden="true">.</span>
			</h2>

			<div class="grid gap-4 sm:grid-cols-3">
				<Field
					label={t('settings.sessionTimeout')}
					id="session-timeout"
					error={fields['/security/session_timeout_minutes']}
				>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							class="field"
							type="number"
							min="1"
							bind:value={settings.security.session_timeout_minutes}
							disabled={!writable}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
				<Field
					label={t('settings.loginRateLimit')}
					id="login-rate"
					error={fields['/security/login_rate_limit_per_minute']}
				>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							class="field"
							type="number"
							min="1"
							bind:value={settings.security.login_rate_limit_per_minute}
							disabled={!writable}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
				<Field
					label={t('settings.apiRateLimit')}
					id="api-rate"
					error={fields['/security/api_rate_limit_per_minute']}
				>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							class="field"
							type="number"
							min="1"
							bind:value={settings.security.api_rate_limit_per_minute}
							disabled={!writable}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
			</div>

			<Field
				label={t('settings.trustedProxies')}
				id="trusted-proxies"
				error={fields['/security/trusted_proxies']}
				hint={t('settings.trustedProxiesHint')}
			>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						class="field"
						placeholder="127.0.0.1, 10.0.0.0/8"
						bind:value={trustedProxies}
						disabled={!writable}
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>

			<label class="flex items-center gap-2 text-sm">
				<input
					type="checkbox"
					class="h-4 w-4 accent-[var(--accent)]"
					bind:checked={settings.security.require_totp}
					disabled={!writable}
				/>
				<span>{t('settings.requireTotp')}</span>
			</label>
		</section>

		<section class="card space-y-4 p-5">
			<div>
				<h2 class="font-semibold">
					{t('settings.telemetry')}<span style="color: var(--color-up)" aria-hidden="true">.</span>
				</h2>
				<p class="muted mt-1 text-sm">{t('settings.telemetryHint')}</p>
			</div>
			<label class="flex items-center gap-2 text-sm">
				<input
					type="checkbox"
					class="h-4 w-4 accent-[var(--accent)]"
					bind:checked={settings.telemetry.enabled}
					disabled={!writable}
				/>
				<span>{t('settings.telemetryEnabled')}</span>
			</label>
		</section>

		{#if writable}
			<Button type="submit" variant="primary" loading={saving} disabled={saving}>
				{t('common.save')}
			</Button>
		{/if}
	</form>
{/if}
