<script lang="ts">
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { api, ApiError } from '$lib/api';
	import type { Group, Monitor, NotificationChannel, Page as ApiPage, Tag } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import { specFor, type FieldSpec } from '$lib/monitortypes';
	import Button from './Button.svelte';
	import Field from './Field.svelte';
	import ErrorBox from './ErrorBox.svelte';

	/**
	 * Create and edit, one form.
	 *
	 * The two differ in three places and nowhere else: the verb, whether the type
	 * can still be changed, and what happens to secrets. That last one is the
	 * interesting case — a monitor is read back with its credentials replaced by
	 * a redaction marker, and the server resolves that marker against the stored
	 * value when it is sent back. So this form deliberately submits the marker it
	 * was given rather than clearing the field: a form that round-trips its own
	 * GET must not destroy the password it was never shown.
	 *
	 * Validation is the server's. Every failure comes back as RFC 9457 with a
	 * JSON pointer per bad field, and those pointers are what highlight the
	 * controls below — so the form cannot disagree with the server about what is
	 * valid, because it never has its own opinion.
	 */
	let { monitor = null }: { monitor?: Monitor | null } = $props();

	const editing = $derived(monitor !== null);

	/**
	 * The prop seeds the form once and then stops mattering: from mount onwards
	 * the fields below are the source of truth, because they are what the person
	 * is typing into. `untrack` says that deliberately rather than leaving the
	 * compiler to warn that a reactive value was read during initialisation.
	 *
	 * Re-seeding on a prop change is the caller's job, and the edit route does it
	 * by mounting this component under `{#key monitor.id}` — a fresh monitor gets
	 * a fresh form rather than having half-typed edits overwritten underneath the
	 * cursor.
	 */
	const seed = untrack(() => ({
		name: monitor?.name ?? '',
		description: monitor?.description ?? '',
		type: monitor?.type ?? 'http',
		enabled: monitor?.enabled ?? true,
		interval: monitor?.interval_seconds ?? 60,
		timeout: monitor?.timeout_seconds ?? 30,
		retries: monitor?.retries ?? 1,
		retryInterval: monitor?.retry_interval_seconds ?? null,
		resendAfter: monitor?.resend_after ?? 0,
		upsideDown: monitor?.upside_down ?? false,
		notifyOnRecovery: monitor?.notify_on_recovery ?? true,
		groupId: monitor?.group_id ?? '',
		tagIds: monitor?.tag_ids ?? [],
		channelIds: monitor ? (monitor.notification_channel_ids ?? []) : null,
		config: { ...(monitor?.config ?? {}) }
	}));

	let name = $state(seed.name);
	let description = $state(seed.description);
	let type = $state(seed.type);
	let enabled = $state(seed.enabled);
	let interval = $state(seed.interval);
	let timeout = $state(seed.timeout);
	let retries = $state(seed.retries);
	let retryInterval = $state<number | null>(seed.retryInterval);
	let resendAfter = $state(seed.resendAfter);
	let upsideDown = $state(seed.upsideDown);
	let notifyOnRecovery = $state(seed.notifyOnRecovery);
	let groupId = $state(seed.groupId);
	let tagIds = $state<string[]>(seed.tagIds);
	let channelIds = $state<string[] | null>(seed.channelIds);
	let config = $state<Record<string, unknown>>(seed.config);
	let rawConfig = $state(JSON.stringify(seed.config, null, 2));

	let groups = $state<Group[]>([]);
	let tags = $state<Tag[]>([]);
	let channels = $state<NotificationChannel[]>([]);

	let saving = $state(false);
	let error = $state<unknown>(null);
	let fieldErrors = $state<Record<string, string>>({});
	let showAdvanced = $state(false);

	const spec = $derived(specFor(type));
	const basic = $derived(spec?.fields.filter((f) => !f.advanced) ?? []);
	const advanced = $derived(spec?.fields.filter((f) => f.advanced) ?? []);

	$effect(() => {
		(async () => {
			try {
				if (session.allows('groups:read')) {
					groups = (await api.get<ApiPage<Group>>('/groups?limit=200')).data;
				}
				if (session.allows('tags:read')) {
					tags = (await api.get<ApiPage<Tag>>('/tags?limit=200')).data;
				}
				if (session.allows('notifications:read')) {
					channels = (
						await api.get<ApiPage<NotificationChannel>>('/notification-channels?limit=200')
					).data;
				}
			} catch {
				// The form still works without them; they only populate optional
				// pickers, and a monitor with no group is the ordinary case.
			}
		})();
	});

	/** Field errors arrive keyed by JSON pointer: `/config/url`, `/interval_seconds`. */
	function errorFor(pointer: string): string | undefined {
		return fieldErrors[pointer];
	}

	function setConfig(key: string, value: unknown) {
		// An emptied optional field is removed rather than sent as "", because ""
		// is a value and the server would validate it as one.
		if (value === '' || value === null || value === undefined) {
			const { [key]: _dropped, ...rest } = config;
			config = rest;
			return;
		}
		config = { ...config, [key]: value };
	}

	function listValue(key: string): string {
		const value = config[key];
		return Array.isArray(value) ? value.join('\n') : '';
	}

	function setList(key: string, text: string) {
		const items = text
			.split('\n')
			.map((line) => line.trim())
			.filter(Boolean);
		setConfig(key, items.length ? items : undefined);
	}

	function onTypeChange(next: string) {
		// The config belongs to the type. Carrying `url` onto a `tcp` monitor would
		// submit a field the checker does not read and cannot reject.
		type = next;
		config = {};
		rawConfig = '{}';
		fieldErrors = {};
	}

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		saving = true;
		error = null;
		fieldErrors = {};

		let payloadConfig: Record<string, unknown>;
		if (spec) {
			payloadConfig = config;
		} else {
			try {
				payloadConfig = JSON.parse(rawConfig);
			} catch {
				fieldErrors = { '/config': 'This is not valid JSON.' };
				saving = false;
				return;
			}
		}

		const body: Record<string, unknown> = {
			name,
			description: description || null,
			config: payloadConfig,
			enabled,
			interval_seconds: interval,
			timeout_seconds: timeout,
			retries,
			retry_interval_seconds: retryInterval,
			resend_after: resendAfter,
			upside_down: upsideDown,
			notify_on_recovery: notifyOnRecovery,
			group_id: groupId || null,
			tag_ids: tagIds
		};
		// `type` is set on create and never changed. A monitor that changed type
		// would carry the previous type's config, and the credential inside it, so
		// the update body has no such field at all — and the server decodes with
		// DisallowUnknownFields, which turns sending one into a 400 on every edit
		// rather than a silently ignored key.
		if (!editing) body.type = type;

		// Absent means "attach the defaults" and an empty array means "deliberately
		// silent"; the two are different and the server distinguishes them. So the
		// key is only sent once somebody has actually chosen.
		if (channelIds !== null) body.notification_channel_ids = channelIds;

		try {
			const saved = editing
				? await api.patch<Monitor>(`/monitors/${monitor!.id}`, body)
				: await api.post<Monitor>('/monitors', body);
			await goto(`/monitors/${saved.id}`, { replaceState: true });
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

	function toggle(values: string[], value: string): string[] {
		return values.includes(value) ? values.filter((v) => v !== value) : [...values, value];
	}

	// Both defined in app.css, so every control in the product shares one
	// treatment rather than each form carrying its own copy of it.
	const inputClass = 'field';
	const inputStyle = '';
</script>

<form class="max-w-3xl space-y-6" onsubmit={submit}>
	{#if error}
		<ErrorBox {error} />
	{/if}

	<section class="card space-y-4 p-5">
		<Field label={t('form.name')} id="name" error={errorFor('/name')}>
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

		<Field label={t('form.description')} id="description" optional error={errorFor('/description')}>
			{#snippet children({ id, describedBy })}
				<input
					{id}
					class={inputClass}
					style={inputStyle}
					bind:value={description}
					aria-describedby={describedBy}
				/>
			{/snippet}
		</Field>

		<Field label={t('form.type')} id="type" error={errorFor('/type')} hint={spec?.summary}>
			{#snippet children({ id, describedBy })}
				<select
					{id}
					class={inputClass}
					style={inputStyle}
					value={type}
					disabled={editing}
					aria-describedby={describedBy}
					onchange={(e) => onTypeChange(e.currentTarget.value)}
				>
					{#each session.info?.monitor_types ?? [type] as option (option)}
						<option value={option}>{specFor(option)?.label ?? option}</option>
					{/each}
				</select>
			{/snippet}
		</Field>
	</section>

	{#if spec === null}
		<!-- A type this build does not describe. Rather than refuse, the config is
		     edited as JSON: the server validates it either way, and a newer server
		     stays usable from an older dashboard. -->
		<section class="card space-y-3 p-5">
			<Field label="Configuration (JSON)" id="raw-config" error={errorFor('/config')}>
				{#snippet children({ id, describedBy, invalid })}
					<textarea
						{id}
						rows="10"
						class="{inputClass} font-mono"
						style={inputStyle}
						bind:value={rawConfig}
						aria-describedby={describedBy}
						aria-invalid={invalid}
					></textarea>
				{/snippet}
			</Field>
		</section>
	{:else if spec.fields.length > 0}
		<section class="card space-y-4 p-5">
			{#each basic as field (field.key)}
				{@render configField(field)}
			{/each}

			{#if advanced.length}
				<div>
					<button
						type="button"
						class="text-sm underline"
						onclick={() => (showAdvanced = !showAdvanced)}
						aria-expanded={showAdvanced}
					>
						{showAdvanced ? '−' : '+'} Advanced
					</button>
					{#if showAdvanced}
						<div class="mt-4 space-y-4">
							{#each advanced as field (field.key)}
								{@render configField(field)}
							{/each}
						</div>
					{/if}
				</div>
			{/if}
		</section>
	{/if}

	<section class="card grid gap-4 p-5 sm:grid-cols-2">
		<Field
			label={t('form.interval')}
			id="interval"
			hint={t('form.seconds')}
			error={errorFor('/interval_seconds')}
		>
			{#snippet children({ id, describedBy, invalid })}
				<input
					{id}
					type="number"
					min="20"
					class={inputClass}
					style={inputStyle}
					bind:value={interval}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				/>
			{/snippet}
		</Field>

		<Field
			label={t('form.timeout')}
			id="timeout"
			hint={t('form.seconds')}
			error={errorFor('/timeout_seconds')}
		>
			{#snippet children({ id, describedBy, invalid })}
				<input
					{id}
					type="number"
					min="1"
					class={inputClass}
					style={inputStyle}
					bind:value={timeout}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				/>
			{/snippet}
		</Field>

		<Field label={t('form.retries')} id="retries" error={errorFor('/retries')}>
			{#snippet children({ id, describedBy, invalid })}
				<input
					{id}
					type="number"
					min="0"
					class={inputClass}
					style={inputStyle}
					bind:value={retries}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				/>
			{/snippet}
		</Field>

		<Field
			label={t('form.retryInterval')}
			id="retry-interval"
			optional
			hint={t('form.seconds')}
			error={errorFor('/retry_interval_seconds')}
		>
			{#snippet children({ id, describedBy })}
				<input
					{id}
					type="number"
					min="1"
					class={inputClass}
					style={inputStyle}
					value={retryInterval ?? ''}
					oninput={(e) =>
						(retryInterval = e.currentTarget.value === '' ? null : Number(e.currentTarget.value))}
					aria-describedby={describedBy}
				/>
			{/snippet}
		</Field>

		<Field label={t('form.resendAfter')} id="resend" error={errorFor('/resend_after')}>
			{#snippet children({ id, describedBy })}
				<input
					{id}
					type="number"
					min="0"
					class={inputClass}
					style={inputStyle}
					bind:value={resendAfter}
					aria-describedby={describedBy}
				/>
			{/snippet}
		</Field>
	</section>

	<section class="card space-y-3 p-5">
		<label class="flex items-center gap-2 text-sm">
			<input type="checkbox" class="h-4 w-4" bind:checked={enabled} />
			{t('form.enabled')}
		</label>
		<label class="flex items-center gap-2 text-sm">
			<input type="checkbox" class="h-4 w-4" bind:checked={notifyOnRecovery} />
			{t('form.notifyOnRecovery')}
		</label>
		<div>
			<label class="flex items-center gap-2 text-sm">
				<input type="checkbox" class="h-4 w-4" bind:checked={upsideDown} />
				{t('form.upsideDown')}
			</label>
			<p class="muted ml-6 text-xs">{t('form.upsideDownHint')}</p>
		</div>
	</section>

	{#if groups.length || tags.length || channels.length}
		<section class="card space-y-4 p-5">
			{#if groups.length}
				<Field label={t('form.group')} id="group" optional error={errorFor('/group_id')}>
					{#snippet children({ id })}
						<select {id} class={inputClass} style={inputStyle} bind:value={groupId}>
							<option value="">{t('common.none')}</option>
							{#each groups as group (group.id)}
								<option value={group.id}>{group.name}</option>
							{/each}
						</select>
					{/snippet}
				</Field>
			{/if}

			{#if tags.length}
				<fieldset>
					<legend class="mb-2 text-sm font-medium">{t('form.tags')}</legend>
					<div class="flex flex-wrap gap-2">
						{#each tags as tag (tag.id)}
							<label
								class="flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs"
								style="border-color: {tagIds.includes(tag.id)
									? 'var(--accent)'
									: 'var(--border-strong)'}"
							>
								<input
									type="checkbox"
									class="h-3 w-3"
									checked={tagIds.includes(tag.id)}
									onchange={() => (tagIds = toggle(tagIds, tag.id))}
								/>
								{tag.name}
							</label>
						{/each}
					</div>
				</fieldset>
			{/if}

			{#if channels.length}
				<fieldset>
					<legend class="text-sm font-medium">{t('form.notificationChannels')}</legend>
					<p class="muted mb-2 text-xs">{t('form.notificationChannelsHint')}</p>
					<div class="space-y-1">
						{#each channels as channel (channel.id)}
							<label class="flex items-center gap-2 text-sm">
								<input
									type="checkbox"
									class="h-4 w-4"
									checked={(channelIds ?? []).includes(channel.id)}
									onchange={() => (channelIds = toggle(channelIds ?? [], channel.id))}
								/>
								{channel.name}
								<span class="muted text-xs">{channel.type}</span>
								{#if channel.is_default}
									<span class="muted text-xs">· default</span>
								{/if}
							</label>
						{/each}
					</div>
				</fieldset>
			{/if}
		</section>
	{/if}

	<div class="flex gap-2">
		<Button type="submit" variant="primary" loading={saving}>
			{editing ? t('common.save') : t('common.create')}
		</Button>
		<Button href={editing ? `/monitors/${monitor!.id}` : '/'} variant="ghost">
			{t('common.cancel')}
		</Button>
	</div>
</form>

{#snippet configField(field: FieldSpec)}
	{@const pointer = `/config/${field.key}`}
	{@const id = `config-${field.key}`}
	<Field
		label={field.label}
		{id}
		optional={!field.required}
		hint={field.hint}
		error={errorFor(pointer)}
	>
		{#snippet children({ id, describedBy, invalid })}
			{#if field.kind === 'boolean'}
				<label class="flex items-center gap-2 text-sm">
					<input
						{id}
						type="checkbox"
						class="h-4 w-4"
						checked={config[field.key] === true}
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
					placeholder={field.placeholder}
					oninput={(e) => setList(field.key, e.currentTarget.value)}
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
					placeholder={field.placeholder}
					oninput={(e) =>
						setConfig(
							field.key,
							e.currentTarget.value === '' ? undefined : Number(e.currentTarget.value)
						)}
					required={field.required}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				/>
			{:else}
				<input
					{id}
					type={field.kind === 'secret' ? 'password' : field.kind === 'url' ? 'url' : 'text'}
					class={inputClass}
					style={inputStyle}
					value={(config[field.key] as string) ?? ''}
					placeholder={field.placeholder}
					oninput={(e) => setConfig(field.key, e.currentTarget.value)}
					required={field.required}
					autocomplete={field.kind === 'secret' ? 'new-password' : undefined}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				/>
			{/if}
		{/snippet}
	</Field>
{/snippet}
