<script lang="ts">
	import { untrack } from 'svelte';
	import { api, ApiError } from '$lib/api';
	import type {
		BrandProfile,
		Group,
		Monitor,
		Page as ApiPage,
		ReportFormat,
		ReportTemplate,
		ReportType,
		Tag
	} from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import Field from '$lib/components/Field.svelte';
	import Button from '$lib/components/Button.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import MonitorPicker from '$lib/components/MonitorPicker.svelte';
	import { session } from '$lib/session.svelte';

	/**
	 * The template editor.
	 *
	 * # Progressive disclosure, and where the line is
	 *
	 * A solo user with three monitors should see a working default and nothing
	 * about SLO targets, error budgets or brand profiles until they ask. The line
	 * this form draws is the **report type**: choosing `uptime` — the default —
	 * hides the target, the budget vocabulary and the comparison entirely,
	 * because none of them applies to that report. Choosing `sla` reveals the
	 * target, and choosing `comparative` reveals the comparison.
	 *
	 * Disclosure by *type* rather than by an "advanced" toggle is the whole
	 * point: an advanced section is a place fields go to be ignored, while the
	 * type is a decision the user has already made and every revealed field
	 * follows from it.
	 *
	 * Brand profiles are hidden when the instance has none, rather than shown
	 * empty. An empty picker is a feature that looks broken; its absence is a
	 * feature that has not been set up.
	 */
	let {
		template = undefined,
		onsaved
	}: {
		template?: ReportTemplate;
		onsaved: (saved: ReportTemplate) => void;
	} = $props();

	const editing = Boolean(template);

	let name = $state(template?.name ?? '');
	let description = $state(template?.description ?? '');
	let type = $state<ReportType>(template?.type ?? 'uptime');
	let period = $state(template?.period ?? 'month');
	let periodStyle = $state(template?.period_style ?? 'calendar');
	let maintenance = $state(template?.maintenance_handling ?? 'exclude');
	let formats = $state<ReportFormat[]>(template?.formats ?? ['pdf', 'html']);

	let slaTarget = $state(template?.sla_target === null ? '' : String(template?.sla_target ?? ''));
	let responseTarget = $state(
		template?.response_time_target_ms === null
			? ''
			: String(template?.response_time_target_ms ?? '')
	);
	let brandProfileID = $state(template?.brand_profile_id ?? '');
	let comparisonMode = $state(template?.comparison?.mode ?? 'previous_period');

	// Scope. Monitors are held with their names so a chosen one can be shown
	// without a second round trip per row.
	let monitors = $state<{ id: string; name: string }[]>([]);
	let groupIDs = $state<string[]>(template?.scope?.group_ids ?? []);
	let tagIDs = $state<string[]>(template?.scope?.tag_ids ?? []);

	let groups = $state<Group[]>([]);
	let tags = $state<Tag[]>([]);
	let brands = $state<BrandProfile[]>([]);

	let saving = $state(false);
	let fieldErrors = $state<Record<string, string>>({});
	let failure = $state<string | null>(null);

	const ALL_FORMATS: ReportFormat[] = ['pdf', 'html', 'csv', 'json'];

	$effect(() => {
		untrack(() => void loadReferences());
	});

	async function loadReferences() {
		const [groupPage, tagPage] = await Promise.all([
			api.get<ApiPage<Group>>('/groups?limit=200').catch(() => ({ data: [] as Group[] })),
			api.get<ApiPage<Tag>>('/tags?limit=200').catch(() => ({ data: [] as Tag[] }))
		]);
		groups = groupPage.data;
		tags = tagPage.data;

		if (session.allows('brand_profiles:read')) {
			brands = (
				await api
					.get<ApiPage<BrandProfile>>('/brand-profiles?limit=100')
					.catch(() => ({ data: [] as BrandProfile[] }))
			).data;
		}

		// Chosen monitors are resolved once, by id, so the picker can show names
		// rather than UUIDs for a template somebody else wrote.
		const chosen = template?.scope?.monitor_ids ?? [];
		if (chosen.length > 0) {
			const resolved = await Promise.all(
				chosen.map((id) =>
					api.get<Monitor>(`/monitors/${id}`).catch(() => ({ id, name: id }) as Monitor)
				)
			);
			monitors = resolved.map((m) => ({ id: m.id, name: m.name }));
		}
	}

	function toggle<T>(list: T[], value: T): T[] {
		return list.includes(value) ? list.filter((item) => item !== value) : [...list, value];
	}

	/**
	 * A number field, or null when it was cleared.
	 *
	 * The distinction matters on a PATCH: absent leaves the value alone and null
	 * removes it, and this form always sends every field, so an emptied box has
	 * to become an explicit null or the target could never be undone.
	 */
	function numberOrNull(raw: string): number | null {
		const trimmed = raw.trim();
		if (trimmed === '') return null;
		const value = Number(trimmed);
		return Number.isFinite(value) ? value : null;
	}

	async function save(event: SubmitEvent) {
		event.preventDefault();
		saving = true;
		fieldErrors = {};
		failure = null;

		const body: Record<string, unknown> = {
			name,
			description: description || null,
			type,
			period,
			period_style: periodStyle,
			maintenance_handling: maintenance,
			formats,
			scope: {
				monitor_ids: monitors.map((m) => m.id),
				group_ids: groupIDs,
				tag_ids: tagIDs
			},
			// Sent as an explicit null when cleared, so removing a target is
			// expressible. Only for the types that have one — an uptime report
			// carries no SLO vocabulary at all, and sending a target it will never
			// read would store configuration nothing reads.
			sla_target: type === 'sla' ? numberOrNull(slaTarget) : null,
			response_time_target_ms: numberOrNull(responseTarget),
			brand_profile_id: brandProfileID || null,
			comparison: type === 'comparative' ? { mode: comparisonMode } : null
		};

		try {
			const saved = editing
				? await api.patch<ReportTemplate>(`/report-templates/${template!.id}`, body)
				: await api.post<ReportTemplate>('/report-templates', body);
			onsaved(saved);
		} catch (caught) {
			if (caught instanceof ApiError) {
				fieldErrors = caught.fields();
				// A problem with no field pointer still has to be shown. The
				// commonest is the comparative rule — a comparison naming fewer
				// than two entities — and swallowing it would leave a form that
				// refuses to save with no reason on the screen.
				if (Object.keys(fieldErrors).length === 0) failure = caught.message;
			} else {
				failure = String(caught);
			}
		} finally {
			saving = false;
		}
	}
</script>

<form class="space-y-6" onsubmit={save}>
	<div class="card space-y-5 p-5">
		<Field label={t('reports.name')} id="report-name" error={fieldErrors['/name']}>
			{#snippet children({ id, describedBy, invalid })}
				<input
					{id}
					class="field"
					bind:value={name}
					required
					maxlength="200"
					aria-describedby={describedBy}
					aria-invalid={invalid}
				/>
			{/snippet}
		</Field>

		<Field label={t('reports.type')} id="report-type" hint={t(`reports.typeHint.${type}`)}>
			{#snippet children({ id, describedBy })}
				<select {id} class="field" bind:value={type} aria-describedby={describedBy}>
					{#each ['uptime', 'sla', 'post_mortem', 'comparative', 'custom'] as option (option)}
						<option value={option}>{t(`reports.type.${option}`)}</option>
					{/each}
				</select>
			{/snippet}
		</Field>

		<div class="grid gap-5 sm:grid-cols-2">
			<Field label={t('reports.period')} id="report-period">
				{#snippet children({ id })}
					<select {id} class="field" bind:value={period}>
						{#each ['day', 'week', 'month', 'quarter', 'year'] as option (option)}
							<option value={option}>{t(`reports.period.${option}`)}</option>
						{/each}
					</select>
				{/snippet}
			</Field>

			<Field label={t('reports.periodStyle')} id="report-style">
				{#snippet children({ id })}
					<select {id} class="field" bind:value={periodStyle}>
						<option value="calendar">{t('reports.periodStyle.calendar')}</option>
						<option value="rolling">{t('reports.periodStyle.rolling')}</option>
					</select>
				{/snippet}
			</Field>
		</div>
	</div>

	<!-- Scope -->
	<div class="card space-y-4 p-5">
		<div>
			<h2 class="text-sm font-medium">{t('reports.scope')}</h2>
			<p class="muted mt-1 text-sm">{t('reports.scopeHint')}</p>
		</div>

		<Field label={t('nav.monitoring')} id="report-monitors" optional>
			{#snippet children({ id })}
				<MonitorPicker
					{id}
					exclude={new Set(monitors.map((m) => m.id))}
					onpick={(monitor) => (monitors = [...monitors, monitor])}
				/>
			{/snippet}
		</Field>

		{#if monitors.length > 0}
			<ul class="flex flex-wrap gap-2">
				{#each monitors as monitor (monitor.id)}
					<li
						class="flex items-center gap-1.5 rounded px-2 py-1 text-sm"
						style="background-color: var(--surface-hover)"
					>
						{monitor.name}
						<button
							type="button"
							aria-label={t('common.remove')}
							onclick={() => (monitors = monitors.filter((m) => m.id !== monitor.id))}
						>
							<Icon name="close" size={14} />
						</button>
					</li>
				{/each}
			</ul>
		{/if}

		{#if groups.length > 0}
			<fieldset>
				<legend class="mb-2 block text-sm font-medium">{t('nav.taxonomy')}</legend>
				<div class="flex flex-wrap gap-2">
					{#each groups as group (group.id)}
						<label class="flex items-center gap-1.5 text-sm">
							<input
								type="checkbox"
								checked={groupIDs.includes(group.id)}
								onchange={() => (groupIDs = toggle(groupIDs, group.id))}
							/>
							{group.name}
						</label>
					{/each}
				</div>
			</fieldset>
		{/if}

		{#if tags.length > 0}
			<fieldset>
				<legend class="mb-2 block text-sm font-medium">{t('tags.title')}</legend>
				<div class="flex flex-wrap gap-2">
					{#each tags as tag (tag.id)}
						<label class="flex items-center gap-1.5 text-sm">
							<input
								type="checkbox"
								checked={tagIDs.includes(tag.id)}
								onchange={() => (tagIDs = toggle(tagIDs, tag.id))}
							/>
							{tag.name}
						</label>
					{/each}
				</div>
			</fieldset>
		{/if}
	</div>

	<!--
		Everything below is revealed by the type. An uptime report — the default —
		shows none of it, which is what "no SLO vocabulary until they ask for a
		target" means in practice.
	-->
	{#if type === 'sla'}
		<div class="card space-y-5 p-5">
			<Field
				label={t('reports.slaTarget')}
				id="report-sla"
				optional
				hint={t('reports.slaTargetHint')}
				error={fieldErrors['/sla_target']}
			>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						class="field"
						type="number"
						step="0.001"
						min="0"
						max="99.999"
						placeholder="99.9"
						bind:value={slaTarget}
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>
		</div>
	{/if}

	{#if type === 'comparative'}
		<div class="card space-y-5 p-5">
			<Field
				label={t('reports.comparison')}
				id="report-comparison"
				hint={t('reports.comparisonHint')}
				error={fieldErrors['/comparison'] ?? fieldErrors['/comparison/mode']}
			>
				{#snippet children({ id, describedBy, invalid })}
					<select
						{id}
						class="field"
						bind:value={comparisonMode}
						aria-describedby={describedBy}
						aria-invalid={invalid}
					>
						{#each ['previous_period', 'monitors', 'groups'] as mode (mode)}
							<option value={mode}>{t(`reports.comparison.${mode}`)}</option>
						{/each}
					</select>
				{/snippet}
			</Field>
		</div>
	{/if}

	<div class="card space-y-5 p-5">
		<Field
			label={t('reports.maintenance')}
			id="report-maintenance"
			hint={t('reports.maintenanceHint')}
		>
			{#snippet children({ id, describedBy })}
				<select {id} class="field" bind:value={maintenance} aria-describedby={describedBy}>
					{#each ['exclude', 'count_as_up', 'count_as_down'] as option (option)}
						<option value={option}>{t(`reports.maintenance.${option}`)}</option>
					{/each}
				</select>
			{/snippet}
		</Field>

		<Field
			label={t('reports.responseTarget')}
			id="report-response"
			optional
			hint={t('reports.responseTargetHint')}
			error={fieldErrors['/response_time_target_ms']}
		>
			{#snippet children({ id, describedBy, invalid })}
				<input
					{id}
					class="field"
					type="number"
					min="1"
					placeholder="500"
					bind:value={responseTarget}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				/>
			{/snippet}
		</Field>

		<fieldset>
			<legend class="mb-2 block text-sm font-medium">{t('reports.formats')}</legend>
			<div class="flex flex-wrap gap-3">
				{#each ALL_FORMATS as format (format)}
					<label class="flex items-center gap-1.5 text-sm">
						<input
							type="checkbox"
							checked={formats.includes(format)}
							onchange={() => (formats = toggle(formats, format))}
						/>
						{format.toUpperCase()}
					</label>
				{/each}
			</div>
			{#if fieldErrors['/formats']}
				<p class="mt-1 text-sm" style="color: var(--color-down)">{fieldErrors['/formats']}</p>
			{/if}
		</fieldset>

		<!--
			Hidden when the instance has no profiles, rather than shown empty: an
			empty picker is a feature that looks broken, while its absence is a
			feature nobody has set up. The report is still branded — it falls back
			to the instance name and the dashboard's colour.
		-->
		{#if brands.length > 0}
			<Field label={t('reports.brand')} id="report-brand" optional>
				{#snippet children({ id })}
					<select {id} class="field" bind:value={brandProfileID}>
						<option value="">{t('reports.brandDefault')}</option>
						{#each brands as brand (brand.id)}
							<option value={brand.id}>{brand.company_name || brand.name}</option>
						{/each}
					</select>
				{/snippet}
			</Field>
		{/if}
	</div>

	{#if failure}
		<p class="text-sm" style="color: var(--color-down)">{failure}</p>
	{/if}

	<div class="flex gap-2">
		<Button type="submit" variant="primary" loading={saving} disabled={saving}>
			{editing ? t('common.save') : t('common.create')}
		</Button>
		<Button href="/reports">{t('common.cancel')}</Button>
	</div>
</form>
