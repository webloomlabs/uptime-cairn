<script lang="ts">
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { api, ApiError } from '$lib/api';
	import type { Monitor, StatusPage } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import Button from './Button.svelte';
	import Field from './Field.svelte';
	import ErrorBox from './ErrorBox.svelte';
	import Icon from './Icon.svelte';
	import MonitorPicker from './MonitorPicker.svelte';

	/**
	 * Creating and editing a status page.
	 *
	 * The page this configures is the one a stranger judges the operator by, so
	 * two of its properties are treated as load-bearing rather than as form
	 * fields: the slug is a public URL that someone may have bookmarked, and the
	 * password is a credential the read path can never hand back. Both are
	 * called out in the form rather than left to be discovered.
	 */
	let { statusPage = null }: { statusPage?: StatusPage | null } = $props();

	const editing = $derived(statusPage !== null);

	type DraftSection = {
		key: number;
		name: string;
		description: string;
		monitorIds: string[];
	};

	let nextKey = 0;

	const seed = untrack(() => ({
		title: statusPage?.title ?? '',
		slug: statusPage?.slug ?? '',
		description: statusPage?.description ?? '',
		published: statusPage?.published ?? false,
		customDomain: statusPage?.custom_domain ?? '',
		visibility: statusPage?.visibility ?? 'public',
		theme: statusPage?.theme ?? 'auto',
		logoURL: statusPage?.logo_url ?? '',
		faviconURL: statusPage?.favicon_url ?? '',
		primaryColor: statusPage?.primary_color ?? '',
		footerText: statusPage?.footer_text ?? '',
		customCSS: statusPage?.custom_css ?? '',
		timezone: statusPage?.timezone ?? '',
		showUptimePercentage: statusPage?.show_uptime_percentage ?? true,
		showResponseTimeChart: statusPage?.show_response_time_chart ?? false,
		uptimeBarDays: statusPage?.uptime_bar_days ?? 90,
		showPoweredBy: statusPage?.show_powered_by ?? true,
		subscriptionsEnabled: statusPage?.subscriptions_enabled ?? false,
		googleAnalyticsID: statusPage?.google_analytics_id ?? '',
		sections: (statusPage?.sections ?? []).map((section) => ({
			key: nextKey++,
			name: section.name,
			description: section.description ?? '',
			monitorIds: [...section.monitor_ids]
		}))
	}));

	let title = $state(seed.title);
	let slug = $state(seed.slug);
	let description = $state(seed.description);
	let published = $state(seed.published);
	let customDomain = $state(seed.customDomain);
	let visibility = $state<'public' | 'password'>(seed.visibility);
	let password = $state('');
	let theme = $state(seed.theme);
	let logoURL = $state(seed.logoURL);
	let faviconURL = $state(seed.faviconURL);
	let primaryColor = $state(seed.primaryColor);
	let footerText = $state(seed.footerText);
	let customCSS = $state(seed.customCSS);
	let timezone = $state(seed.timezone);
	let showUptimePercentage = $state(seed.showUptimePercentage);
	let showResponseTimeChart = $state(seed.showResponseTimeChart);
	let uptimeBarDays = $state(seed.uptimeBarDays);
	let showPoweredBy = $state(seed.showPoweredBy);
	let subscriptionsEnabled = $state(seed.subscriptionsEnabled);
	let googleAnalyticsID = $state(seed.googleAnalyticsID);
	let sections = $state<DraftSection[]>(seed.sections);

	let saving = $state(false);
	let error = $state<unknown>(null);
	let fieldErrors = $state<Record<string, string>>({});

	/**
	 * Names for the monitors already on the page.
	 *
	 * The list endpoint has no filter by id, so these are resolved one at a time
	 * — bounded by how many monitors an operator has actually placed, not by how
	 * many exist. A 404 and a failed request are kept apart because they license
	 * different claims: one means the monitor is gone and the row should be
	 * removed, the other means this browser could not find out.
	 */
	type Resolved = { name: string } | { missing: true } | { failed: true };
	let names = $state<Record<string, Resolved>>({});

	// A slug is a public URL. It is derived from the title while nobody has
	// touched it, and left alone the moment somebody does — including on every
	// edit of an existing page, where changing it would break a bookmark.
	let slugTouched = $state(seed.slug !== '');

	const usedIDs = $derived(new Set(sections.flatMap((section) => section.monitorIds)));

	const timezones = untrack(() => {
		try {
			return (Intl as { supportedValuesOf?: (key: string) => string[] }).supportedValuesOf?.(
				'timeZone'
			) as string[] | undefined;
		} catch {
			return undefined;
		}
	});

	// Tracks the selected ids and deliberately does not track `names`, which this
	// effect writes. Reading both would make it invalidate itself on every
	// resolution — the self-feeding effect loop that takes a dashboard down.
	$effect(() => {
		const ids = [...usedIDs];
		const missing = untrack(() =>
			ids.filter((id) => !Object.prototype.hasOwnProperty.call(names, id))
		);
		if (missing.length === 0) return;
		void resolveNames(missing);
	});

	async function resolveNames(ids: string[]) {
		const resolved = await Promise.all(
			ids.map(async (id) => {
				try {
					const monitor = await api.get<Monitor>(`/monitors/${id}`);
					return [id, { name: monitor.name }] as const;
				} catch (caught) {
					const gone = caught instanceof ApiError && caught.status === 404;
					return [id, gone ? { missing: true as const } : { failed: true as const }] as const;
				}
			})
		);
		names = { ...names, ...Object.fromEntries(resolved) };
	}

	function slugify(value: string): string {
		return value
			.toLowerCase()
			.normalize('NFKD')
			.replace(/[^a-z0-9]+/g, '-')
			.replace(/^-+/, '')
			.slice(0, 64)
			.replace(/-+$/, '');
	}

	function onTitleInput(value: string) {
		title = value;
		if (!slugTouched) slug = slugify(value);
	}

	function addSection() {
		sections = [...sections, { key: nextKey++, name: '', description: '', monitorIds: [] }];
	}

	function removeSection(index: number) {
		sections = sections.filter((_, i) => i !== index);
	}

	function moveSection(index: number, delta: number) {
		const target = index + delta;
		if (target < 0 || target >= sections.length) return;
		const next = [...sections];
		[next[index], next[target]] = [next[target], next[index]];
		sections = next;
	}

	function addMonitor(index: number, monitor: { id: string; name: string }) {
		names = { ...names, [monitor.id]: { name: monitor.name } };
		sections = sections.map((section, i) =>
			i === index ? { ...section, monitorIds: [...section.monitorIds, monitor.id] } : section
		);
	}

	function removeMonitor(index: number, id: string) {
		sections = sections.map((section, i) =>
			i === index
				? { ...section, monitorIds: section.monitorIds.filter((held) => held !== id) }
				: section
		);
	}

	function moveMonitor(index: number, position: number, delta: number) {
		const target = position + delta;
		const held = sections[index].monitorIds;
		if (target < 0 || target >= held.length) return;
		const next = [...held];
		[next[position], next[target]] = [next[target], next[position]];
		sections = sections.map((section, i) =>
			i === index ? { ...section, monitorIds: next } : section
		);
	}

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		saving = true;
		error = null;
		fieldErrors = {};

		const body: Record<string, unknown> = {
			slug,
			title,
			// These fields are nullable in the spec but the server reads them as
			// "absent means leave alone". An empty string is what clears one —
			// sending null would silently keep the old value.
			description,
			published,
			custom_domain: customDomain.trim() || null,
			visibility,
			theme,
			logo_url: logoURL.trim(),
			favicon_url: faviconURL.trim(),
			primary_color: primaryColor.trim(),
			footer_text: footerText,
			custom_css: customCSS,
			timezone: timezone.trim(),
			show_uptime_percentage: showUptimePercentage,
			show_response_time_chart: showResponseTimeChart,
			uptime_bar_days: uptimeBarDays,
			show_powered_by: showPoweredBy,
			subscriptions_enabled: subscriptionsEnabled,
			google_analytics_id: googleAnalyticsID.trim(),
			sections: sections.map((section) => ({
				name: section.name,
				description: section.description || null,
				monitor_ids: section.monitorIds
			}))
		};

		// The password is write-only and the read path cannot return it, so an
		// untouched box means "keep what is stored" rather than "clear it".
		// Turning the page public does clear it: keeping a credential for a mode
		// that is switched off stores a secret nothing will ever check.
		if (visibility === 'public') body.password = null;
		else if (password !== '') body.password = password;

		try {
			const saved = editing
				? await api.patch<StatusPage>(`/status-pages/${statusPage!.id}`, body)
				: await api.post<StatusPage>('/status-pages', body);
			await goto(`/status-pages/${saved.id}/edit`, { replaceState: true });
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

	const inputClass = 'field';
</script>

<form class="space-y-5" onsubmit={submit}>
	{#if error}
		<ErrorBox {error} />
	{/if}

	<section class="card space-y-4 p-5">
		<h2 class="font-semibold">{t('statusPages.basics')}</h2>

		<div class="grid gap-4 sm:grid-cols-2">
			<Field label={t('form.name')} id="page-title" error={fieldErrors['/title']}>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						class={inputClass}
						value={title}
						oninput={(e) => onTitleInput(e.currentTarget.value)}
						required
						maxlength="200"
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>

			<Field
				label={t('statusPages.slug')}
				id="page-slug"
				error={fieldErrors['/slug']}
				hint={editing ? t('statusPages.slugEditHint') : t('statusPages.slugHint')}
			>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						class="{inputClass} font-mono"
						value={slug}
						oninput={(e) => {
							slugTouched = true;
							slug = e.currentTarget.value;
						}}
						required
						maxlength="64"
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>
		</div>

		<Field
			label={t('form.description')}
			id="page-description"
			optional
			error={fieldErrors['/description']}
		>
			{#snippet children({ id, describedBy, invalid })}
				<textarea
					{id}
					rows="2"
					class={inputClass}
					bind:value={description}
					maxlength="5000"
					aria-describedby={describedBy}
					aria-invalid={invalid}
				></textarea>
			{/snippet}
		</Field>

		<label class="flex items-start gap-2 text-sm">
			<input type="checkbox" class="mt-0.5 h-4 w-4" bind:checked={published} />
			<span>
				{t('statusPages.publishedLabel')}
				<span class="muted block text-xs">{t('statusPages.publishedHint')}</span>
			</span>
		</label>
	</section>

	<section class="card space-y-4 p-5">
		<div class="flex flex-wrap items-center justify-between gap-2">
			<h2 class="font-semibold">{t('statusPages.sections')}</h2>
			<Button size="sm" onclick={addSection}>
				<Icon name="plus" size={14} />
				{t('statusPages.addSection')}
			</Button>
		</div>
		<p class="muted text-sm">{t('statusPages.sectionsHint')}</p>

		{#if fieldErrors['/sections']}
			<p class="text-sm" style="color: var(--color-down)">{fieldErrors['/sections']}</p>
		{/if}

		{#if sections.length === 0}
			<p
				class="muted rounded-lg border border-dashed px-4 py-8 text-center text-sm"
				style="border-color: var(--border)"
			>
				{t('statusPages.noSections')}
			</p>
		{/if}

		{#each sections as section, index (section.key)}
			<div class="space-y-3 rounded-lg border p-4" style="border-color: var(--border)">
				<div class="flex flex-wrap items-end gap-3">
					<div class="min-w-[12rem] flex-1">
						<Field
							label={t('statusPages.sectionName')}
							id="section-name-{section.key}"
							error={fieldErrors[`/sections/${index}/name`]}
						>
							{#snippet children({ id, describedBy, invalid })}
								<input
									{id}
									class={inputClass}
									value={section.name}
									oninput={(e) => (sections[index].name = e.currentTarget.value)}
									required
									maxlength="200"
									aria-describedby={describedBy}
									aria-invalid={invalid}
								/>
							{/snippet}
						</Field>
					</div>
					<div class="flex gap-1">
						<Button
							size="sm"
							variant="ghost"
							onclick={() => moveSection(index, -1)}
							disabled={index === 0}
							title={t('statusPages.moveUp')}
						>
							↑
						</Button>
						<Button
							size="sm"
							variant="ghost"
							onclick={() => moveSection(index, 1)}
							disabled={index === sections.length - 1}
							title={t('statusPages.moveDown')}
						>
							↓
						</Button>
						<Button size="sm" variant="danger" onclick={() => removeSection(index)}>
							{t('common.delete')}
						</Button>
					</div>
				</div>

				<Field label={t('form.description')} id="section-description-{section.key}" optional>
					{#snippet children({ id })}
						<input
							{id}
							class={inputClass}
							value={section.description}
							oninput={(e) => (sections[index].description = e.currentTarget.value)}
							maxlength="2000"
						/>
					{/snippet}
				</Field>

				<div class="space-y-2">
					<p class="text-sm font-medium">{t('statusPages.monitors')}</p>
					{#if section.monitorIds.length === 0}
						<p class="muted text-sm">{t('statusPages.sectionEmpty')}</p>
					{:else}
						<ul class="space-y-1">
							{#each section.monitorIds as monitorId, position (monitorId)}
								<li
									class="flex items-center gap-2 rounded-lg px-2.5 py-1.5 text-sm"
									style="background-color: var(--surface-hover)"
								>
									<span class="min-w-0 flex-1 truncate">
										{#if names[monitorId] === undefined}
											<span class="muted">{t('common.loading')}</span>
										{:else if 'name' in names[monitorId]}
											{names[monitorId].name}
										{:else}
											<!-- Two different facts, kept apart. A monitor the server
											     says is gone can be removed with confidence; one this
											     browser merely failed to read cannot. -->
											<span
												style="color: var({'missing' in names[monitorId]
													? '--color-down'
													: '--color-pending'})"
											>
												{'missing' in names[monitorId]
													? t('statusPages.missingMonitor')
													: t('statusPages.unresolvedMonitor')}
											</span>
											<span class="muted font-mono text-xs">{monitorId}</span>
										{/if}
									</span>
									<button
										type="button"
										class="muted rounded p-1 hover:bg-[var(--surface-raised)]"
										onclick={() => moveMonitor(index, position, -1)}
										disabled={position === 0}
										aria-label={t('statusPages.moveUp')}
									>
										↑
									</button>
									<button
										type="button"
										class="muted rounded p-1 hover:bg-[var(--surface-raised)]"
										onclick={() => moveMonitor(index, position, 1)}
										disabled={position === section.monitorIds.length - 1}
										aria-label={t('statusPages.moveDown')}
									>
										↓
									</button>
									<button
										type="button"
										class="rounded p-1 hover:bg-[var(--color-down-soft)]"
										style="color: var(--color-down)"
										onclick={() => removeMonitor(index, monitorId)}
										aria-label={t('statusPages.removeMonitor')}
									>
										<Icon name="trash" size={14} />
									</button>
								</li>
							{/each}
						</ul>
					{/if}

					<MonitorPicker
						id="section-picker-{section.key}"
						exclude={usedIDs}
						onpick={(monitor) => addMonitor(index, monitor)}
					/>
				</div>
			</div>
		{/each}
	</section>

	<section class="card space-y-4 p-5">
		<h2 class="font-semibold">{t('statusPages.access')}</h2>

		<div class="grid gap-4 sm:grid-cols-2">
			<Field
				label={t('statusPages.visibilityLabel')}
				id="page-visibility"
				error={fieldErrors['/visibility']}
			>
				{#snippet children({ id, describedBy })}
					<select {id} class={inputClass} bind:value={visibility} aria-describedby={describedBy}>
						<option value="public">{t('statusPages.visibility.public')}</option>
						<option value="password">{t('statusPages.visibility.password')}</option>
					</select>
				{/snippet}
			</Field>

			{#if visibility === 'password'}
				<Field
					label={t('statusPages.password')}
					id="page-password"
					error={fieldErrors['/password']}
					hint={editing ? t('statusPages.passwordEditHint') : t('statusPages.passwordHint')}
				>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							type="password"
							class={inputClass}
							bind:value={password}
							autocomplete="new-password"
							minlength="8"
							required={!editing}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
			{/if}
		</div>

		<Field
			label={t('statusPages.customDomain')}
			id="page-domain"
			optional
			error={fieldErrors['/custom_domain']}
			hint={t('statusPages.customDomainHint')}
		>
			{#snippet children({ id, describedBy, invalid })}
				<input
					{id}
					class={inputClass}
					bind:value={customDomain}
					placeholder="status.example.com"
					aria-describedby={describedBy}
					aria-invalid={invalid}
				/>
			{/snippet}
		</Field>
	</section>

	<section class="card space-y-4 p-5">
		<h2 class="font-semibold">{t('statusPages.appearance')}</h2>

		<div class="grid gap-4 sm:grid-cols-2">
			<Field label={t('statusPages.theme')} id="page-theme" error={fieldErrors['/theme']}>
				{#snippet children({ id })}
					<select {id} class={inputClass} bind:value={theme}>
						<option value="auto">{t('theme.system')}</option>
						<option value="light">{t('theme.light')}</option>
						<option value="dark">{t('theme.dark')}</option>
					</select>
				{/snippet}
			</Field>

			<Field
				label={t('statusPages.primaryColor')}
				id="page-color"
				optional
				error={fieldErrors['/primary_color']}
			>
				{#snippet children({ id, describedBy, invalid })}
					<div class="flex gap-2">
						<input
							{id}
							class="{inputClass} font-mono"
							bind:value={primaryColor}
							placeholder="#4f46e5"
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
						<input
							type="color"
							class="h-10 w-12 shrink-0 cursor-pointer rounded-lg border"
							style="border-color: var(--border); background-color: var(--surface-raised)"
							value={/^#[0-9a-fA-F]{6}$/.test(primaryColor) ? primaryColor : '#4f46e5'}
							oninput={(e) => (primaryColor = e.currentTarget.value)}
							aria-label={t('statusPages.primaryColor')}
						/>
					</div>
				{/snippet}
			</Field>

			<Field
				label={t('statusPages.logoURL')}
				id="page-logo"
				optional
				error={fieldErrors['/logo_url']}
			>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						type="url"
						class={inputClass}
						bind:value={logoURL}
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>

			<Field
				label={t('statusPages.faviconURL')}
				id="page-favicon"
				optional
				error={fieldErrors['/favicon_url']}
			>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						type="url"
						class={inputClass}
						bind:value={faviconURL}
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>
		</div>

		<Field
			label={t('statusPages.footerText')}
			id="page-footer"
			optional
			error={fieldErrors['/footer_text']}
		>
			{#snippet children({ id, describedBy, invalid })}
				<textarea
					{id}
					rows="2"
					class={inputClass}
					bind:value={footerText}
					maxlength="2000"
					aria-describedby={describedBy}
					aria-invalid={invalid}
				></textarea>
			{/snippet}
		</Field>
	</section>

	<section class="card space-y-4 p-5">
		<h2 class="font-semibold">{t('statusPages.display')}</h2>

		<div class="grid gap-4 sm:grid-cols-2">
			<Field
				label={t('statusPages.uptimeBarDays')}
				id="page-bar-days"
				error={fieldErrors['/uptime_bar_days']}
				hint={t('statusPages.uptimeBarDaysHint')}
			>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						type="number"
						min="7"
						max="365"
						class={inputClass}
						bind:value={uptimeBarDays}
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>

			<Field
				label={t('statusPages.timezone')}
				id="page-timezone"
				optional
				error={fieldErrors['/timezone']}
				hint={t('statusPages.timezoneHint')}
			>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						class={inputClass}
						bind:value={timezone}
						list={timezones ? 'iana-timezones' : undefined}
						placeholder="UTC"
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>
		</div>

		{#if timezones}
			<datalist id="iana-timezones">
				{#each timezones as zone (zone)}
					<option value={zone}></option>
				{/each}
			</datalist>
		{/if}

		<div class="space-y-2">
			<label class="flex items-center gap-2 text-sm">
				<input type="checkbox" class="h-4 w-4" bind:checked={showUptimePercentage} />
				{t('statusPages.showUptime')}
			</label>
			<label class="flex items-center gap-2 text-sm">
				<input type="checkbox" class="h-4 w-4" bind:checked={showResponseTimeChart} />
				{t('statusPages.showResponseChart')}
			</label>
			<label class="flex items-start gap-2 text-sm">
				<input type="checkbox" class="mt-0.5 h-4 w-4" bind:checked={showPoweredBy} />
				<span>
					{t('statusPages.showPoweredBy')}
					<span class="muted block text-xs">{t('statusPages.showPoweredByHint')}</span>
				</span>
			</label>
		</div>
	</section>

	<section class="card space-y-4 p-5">
		<h2 class="font-semibold">{t('statusPages.subscriptions')}</h2>

		<label class="flex items-start gap-2 text-sm">
			<input type="checkbox" class="mt-0.5 h-4 w-4" bind:checked={subscriptionsEnabled} />
			<span>
				{t('statusPages.subscriptionsLabel')}
				<span class="muted block text-xs">{t('statusPages.subscriptionsHint')}</span>
			</span>
		</label>

		{#if subscriptionsEnabled && session.info && !session.can('subscriber_delivery')}
			<!-- Said before saving rather than discovered afterwards: a subscribe box
			     the install cannot honour is worse than none, because the person who
			     used it believes they will be told. -->
			<p
				class="rounded-lg px-3 py-2 text-sm"
				style="background-color: var(--color-pending-soft)"
				role="status"
			>
				{t('statusPages.deliveryOff')}
			</p>
		{/if}
	</section>

	<details class="card p-5">
		<summary class="cursor-pointer font-semibold">{t('statusPages.advanced')}</summary>
		<div class="mt-4 space-y-4">
			<Field
				label={t('statusPages.customCSS')}
				id="page-css"
				optional
				error={fieldErrors['/custom_css']}
				hint={t('statusPages.customCSSHint')}
			>
				{#snippet children({ id, describedBy, invalid })}
					<textarea
						{id}
						rows="6"
						class="{inputClass} font-mono text-xs"
						bind:value={customCSS}
						aria-describedby={describedBy}
						aria-invalid={invalid}
					></textarea>
				{/snippet}
			</Field>

			<Field
				label={t('statusPages.analyticsID')}
				id="page-analytics"
				optional
				error={fieldErrors['/google_analytics_id']}
			>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						class={inputClass}
						bind:value={googleAnalyticsID}
						placeholder="G-XXXXXXXXXX"
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>
		</div>
	</details>

	<div class="flex flex-wrap gap-2">
		<Button type="submit" variant="primary" loading={saving}>
			{editing ? t('common.save') : t('common.create')}
		</Button>
		<Button variant="ghost" href="/status-pages">{t('common.cancel')}</Button>
	</div>
</form>
