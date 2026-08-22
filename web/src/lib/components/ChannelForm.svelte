<script lang="ts">
	import { untrack } from 'svelte';
	import { api, ApiError } from '$lib/api';
	import type { NotificationChannel } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import { channelSpec, type ChannelFieldSpec } from '$lib/channeltypes';
	import Button from './Button.svelte';
	import Field from './Field.svelte';
	import ErrorBox from './ErrorBox.svelte';
	import TemplatePreview from './TemplatePreview.svelte';

	/**
	 * A notification channel, with the two things that make one trustworthy:
	 * a test that actually sends, and a preview that renders through the same
	 * code delivery does.
	 *
	 * Test-fire is a real delivery, not a simulation. That is the point — the
	 * failure this feature cannot be allowed to have is a channel that has quietly
	 * stopped working, and a test that stops short of the provider would not catch
	 * it. So the result reports what the provider said, verbatim.
	 */
	let {
		channel = null,
		onsaved,
		oncancel
	}: {
		channel?: NotificationChannel | null;
		onsaved: () => void;
		oncancel: () => void;
	} = $props();

	const editing = $derived(channel !== null);

	const seed = untrack(() => ({
		name: channel?.name ?? '',
		type: channel?.type ?? 'email',
		enabled: channel?.enabled ?? true,
		isDefault: channel?.is_default ?? false,
		config: { ...(channel?.config ?? {}) }
	}));

	let name = $state(seed.name);
	let type = $state(seed.type);
	let enabled = $state(seed.enabled);
	let isDefault = $state(seed.isDefault);
	let config = $state<Record<string, unknown>>(seed.config);

	let saving = $state(false);
	let error = $state<unknown>(null);
	let fieldErrors = $state<Record<string, string>>({});
	let showAdvanced = $state(false);

	let testing = $state(false);
	let testResult = $state<{
		delivered: boolean;
		status_code?: number | null;
		duration_ms: number;
		error?: string | null;
		rendered_payload?: string | null;
	} | null>(null);

	const spec = $derived(channelSpec(type));
	const basic = $derived(spec?.fields.filter((f) => !f.advanced) ?? []);
	const advanced = $derived(spec?.fields.filter((f) => f.advanced) ?? []);
	const templateFields = $derived(spec?.fields.filter((f) => f.template) ?? []);

	// The channel types this build actually has. Apprise is the case that
	// matters: it is compiled in and useless without the binary installed, so
	// offering it on a host without one is offering a channel that fails on first
	// use — and /system/info already answers that honestly.
	const available = $derived(
		(session.info?.notification_channel_types ?? []).filter(
			(candidate) => candidate !== 'apprise' || session.can('apprise')
		)
	);

	function setConfig(key: string, value: unknown) {
		if (value === '' || value === null || value === undefined) {
			const { [key]: _dropped, ...rest } = config;
			config = rest;
			return;
		}
		config = { ...config, [key]: value };
	}

	/**
	 * What a boolean field is actually set to right now.
	 *
	 * An absent key does not mean off — it means the server will apply its own
	 * default, and several of them default to on (`use_instance_smtp`,
	 * `verify_tls`, `auto_resolve`). Rendering absence as an empty checkbox showed
	 * the opposite of what was about to happen, and for the instance relay it made
	 * the first save fail on a box the user could see was already unticked.
	 */
	function flag(field: ChannelFieldSpec): boolean {
		const value = config[field.key];
		return typeof value === 'boolean' ? value : (field.default ?? false);
	}

	function listValue(key: string): string {
		const value = config[key];
		return Array.isArray(value) ? value.join('\n') : '';
	}

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		saving = true;
		error = null;
		fieldErrors = {};

		const body: Record<string, unknown> = {
			name,
			config,
			enabled,
			is_default: isDefault
		};
		// `type` is set on create and never changed: a channel that changed type
		// would carry the previous type's config, and the credential inside it.
		if (!editing) body.type = type;

		try {
			if (editing) await api.patch(`/notification-channels/${channel!.id}`, body);
			else await api.post('/notification-channels', body);
			onsaved();
		} catch (caught) {
			if (caught instanceof ApiError && caught.problem.errors?.length) {
				fieldErrors = caught.fields();
				error = new ApiError({ ...caught.problem, detail: t('form.fixFields') });
			} else {
				error = caught;
			}
		} finally {
			saving = false;
		}
	}

	async function fire() {
		if (!channel) return;
		testing = true;
		testResult = null;
		try {
			testResult = await api.post(`/notification-channels/${channel.id}/test`, {
				sample_event: 'monitor.down'
			});
		} catch (caught) {
			error = caught;
		} finally {
			testing = false;
		}
	}

	// Both defined in app.css, so every control in the product shares one
	// treatment rather than each form carrying its own copy of it.
	const inputClass = 'field';
	const inputStyle = '';
</script>

<form class="space-y-5" onsubmit={submit}>
	{#if error}
		<ErrorBox {error} />
	{/if}

	<div class="grid gap-4 sm:grid-cols-2">
		<Field label={t('form.name')} id="channel-name" error={fieldErrors['/name']}>
			{#snippet children({ id, describedBy, invalid })}
				<input
					{id}
					class={inputClass}
					style={inputStyle}
					bind:value={name}
					required
					aria-describedby={describedBy}
					aria-invalid={invalid}
				/>
			{/snippet}
		</Field>

		<Field
			label={t('form.type')}
			id="channel-type"
			error={fieldErrors['/type']}
			hint={spec?.summary}
		>
			{#snippet children({ id, describedBy })}
				<select
					{id}
					class={inputClass}
					style={inputStyle}
					value={type}
					disabled={editing}
					aria-describedby={describedBy}
					onchange={(e) => {
						type = e.currentTarget.value;
						config = {};
						fieldErrors = {};
					}}
				>
					{#each available.length ? available : [type] as option (option)}
						<option value={option}>{channelSpec(option)?.label ?? option}</option>
					{/each}
				</select>
			{/snippet}
		</Field>
	</div>

	{#if spec}
		<div class="space-y-4">
			{#each basic as field (field.key)}
				{@render channelField(field)}
			{/each}

			{#if advanced.length}
				<button
					type="button"
					class="text-sm underline"
					onclick={() => (showAdvanced = !showAdvanced)}
					aria-expanded={showAdvanced}
				>
					{showAdvanced ? '−' : '+'} Advanced
				</button>
				{#if showAdvanced}
					<div class="space-y-4">
						{#each advanced as field (field.key)}
							{@render channelField(field)}
						{/each}
					</div>
				{/if}
			{/if}
		</div>
	{/if}

	<div class="space-y-2">
		<label class="flex items-center gap-2 text-sm">
			<input type="checkbox" class="h-4 w-4" bind:checked={enabled} />
			{t('form.enabled')}
		</label>
		<label class="flex items-center gap-2 text-sm">
			<input type="checkbox" class="h-4 w-4" bind:checked={isDefault} />
			Attach to new monitors by default
		</label>
	</div>

	{#each templateFields as field (field.key)}
		{#if typeof config[field.key] === 'string' && config[field.key] !== ''}
			<TemplatePreview template={config[field.key] as string} label={field.label} />
		{/if}
	{/each}

	<div class="flex flex-wrap gap-2">
		<Button type="submit" variant="primary" loading={saving}>
			{editing ? t('common.save') : t('common.create')}
		</Button>
		{#if editing}
			<Button loading={testing} onclick={fire}>Send a test</Button>
		{/if}
		<Button variant="ghost" onclick={oncancel}>{t('common.cancel')}</Button>
	</div>

	{#if testResult}
		<!-- A real delivery, reported as one. The provider's own error text is
		     passed through verbatim by the server, because a summarised provider
		     error is a support ticket and the real one is usually the answer. -->
		<div
			class="space-y-1 rounded-md px-4 py-3 text-sm"
			style="background-color: var(--color-{testResult.delivered ? 'up' : 'down'}-soft)"
			role="status"
		>
			<p class="font-medium">
				{testResult.delivered ? 'Delivered' : 'Not delivered'}
				<span class="font-normal">· {Math.round(testResult.duration_ms)} ms</span>
				{#if testResult.status_code}
					<span class="font-normal">· HTTP {testResult.status_code}</span>
				{/if}
			</p>
			{#if testResult.error}
				<p class="font-mono text-xs break-all">{testResult.error}</p>
			{/if}
		</div>
	{/if}
</form>

{#snippet channelField(field: ChannelFieldSpec)}
	{@const pointer = `/config/${field.key}`}
	{@const id = `channel-config-${field.key}`}
	<Field
		label={field.label}
		{id}
		optional={!field.required}
		hint={field.hint}
		error={fieldErrors[pointer]}
	>
		{#snippet children({ id, describedBy, invalid })}
			{#if field.kind === 'boolean'}
				<label class="flex items-center gap-2 text-sm">
					<input
						{id}
						type="checkbox"
						class="h-4 w-4"
						checked={flag(field)}
						onchange={(e) => setConfig(field.key, e.currentTarget.checked)}
						aria-describedby={describedBy}
					/>
					{field.label}
				</label>
			{:else if field.kind === 'select'}
				<select
					{id}
					class={inputClass}
					style={inputStyle}
					value={(config[field.key] as string) ?? ''}
					onchange={(e) => setConfig(field.key, e.currentTarget.value)}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				>
					{#each field.options ?? [] as option (option)}
						<option value={option}>{option === '' ? t('common.none') : option}</option>
					{/each}
				</select>
			{:else if field.kind === 'list'}
				<textarea
					{id}
					rows="3"
					class={inputClass}
					style={inputStyle}
					value={listValue(field.key)}
					oninput={(e) => {
						const items = e.currentTarget.value
							.split('\n')
							.map((line) => line.trim())
							.filter(Boolean);
						setConfig(field.key, items.length ? items : undefined);
					}}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				></textarea>
			{:else if field.kind === 'number'}
				<input
					{id}
					type="number"
					min={field.min}
					max={field.max}
					class={inputClass}
					style={inputStyle}
					value={(config[field.key] as number) ?? ''}
					oninput={(e) =>
						setConfig(
							field.key,
							e.currentTarget.value === '' ? undefined : Number(e.currentTarget.value)
						)}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				/>
			{:else if field.template}
				<textarea
					{id}
					rows="4"
					class="{inputClass} font-mono"
					style={inputStyle}
					value={(config[field.key] as string) ?? ''}
					oninput={(e) => setConfig(field.key, e.currentTarget.value)}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				></textarea>
			{:else}
				<input
					{id}
					type={field.secret ? 'password' : field.kind === 'url' ? 'url' : 'text'}
					class={inputClass}
					style={inputStyle}
					value={(config[field.key] as string) ?? ''}
					oninput={(e) => setConfig(field.key, e.currentTarget.value)}
					required={field.required && !editing}
					autocomplete={field.secret ? 'new-password' : undefined}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				/>
			{/if}
		{/snippet}
	</Field>
{/snippet}
