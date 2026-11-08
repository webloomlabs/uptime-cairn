<script lang="ts">
	import { goto } from '$app/navigation';
	import { api, ApiError } from '$lib/api';
	import type {
		Group,
		MaintenanceStrategy,
		MaintenanceWindow,
		Page as ApiPage,
		StatusPage,
		Tag
	} from '$lib/types';
	import { MAINTENANCE_STRATEGIES } from '$lib/types';
	import { i18n, t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import {
		addMinutes,
		browserZone,
		fromZonedInput,
		knownZones,
		nowInZone,
		toZonedInput
	} from '$lib/zoned';
	import Field from './Field.svelte';
	import Button from './Button.svelte';
	import ErrorBox from './ErrorBox.svelte';
	import MonitorPicker from './MonitorPicker.svelte';

	/**
	 * Creating and editing a maintenance window.
	 *
	 * Times are entered as wall-clock time in the window's own zone, which is
	 * what the server evaluates them in: "02:00 every Sunday" has to mean 02:00
	 * where the servers are, whoever is filling in the form. Validation is the
	 * server's, and its JSON pointers highlight the controls.
	 *
	 * Status pages default to automatic: the form sends `auto_status_pages` and
	 * the server puts the window on every page that shows a monitor it covers.
	 * Choosing pages by hand sends `status_page_ids` as given, an empty one
	 * included.
	 */
	let {
		maintenance: existing = undefined,
		monitorIds: initialMonitorIds = []
	}: { maintenance?: MaintenanceWindow; monitorIds?: string[] } = $props();

	const editing = existing !== undefined;

	// Seeded once from the props and then owned by the form, as IncidentForm
	// does: a re-fetch must not overwrite what somebody is halfway through typing.
	let title = $state(existing?.title ?? '');
	let description = $state(existing?.description ?? '');
	let strategy = $state<MaintenanceStrategy>(existing?.strategy ?? 'single');
	let timezone = $state(existing?.timezone ?? browserZone());
	const zone = existing?.timezone ?? browserZone();
	let startsAt = $state(existing ? toZonedInput(existing.starts_at, zone) : nowInZone(zone));
	let endsAt = $state(
		existing
			? toZonedInput(
					existing.ends_at ??
						(existing.duration_minutes
							? new Date(
									Date.parse(existing.starts_at) + existing.duration_minutes * 60_000
								).toISOString()
							: null),
					zone
				)
			: addMinutes(nowInZone(zone), 60, zone)
	);
	let durationMinutes = $state<number>(existing?.duration_minutes ?? 60);
	let weekdays = $state<number[]>(existing?.recurrence?.weekdays ?? []);
	let daysOfMonth = $state<number[]>(existing?.recurrence?.days_of_month ?? []);
	let cron = $state(existing?.recurrence?.cron ?? '');
	let until = $state(toZonedInput(existing?.recurrence?.until, zone));

	let picked = $state<{ id: string; name: string }[]>([]);
	const chosen = $derived(new Set(picked.map((m) => m.id)));
	let groupIds = $state<string[]>(existing?.targets.group_ids ?? []);
	let tagIds = $state<string[]>(existing?.targets.tag_ids ?? []);

	let suppress = $state(existing?.suppress_notifications ?? true);
	let showOnPages = $state(existing?.show_on_status_pages ?? true);
	let pagesMode = $state<'auto' | 'choose'>(editing ? 'choose' : 'auto');
	let pageIds = $state<string[]>(existing?.status_page_ids ?? []);

	let groups = $state<Group[]>([]);
	let tags = $state<Tag[]>([]);
	let pages = $state<StatusPage[]>([]);
	const zones = knownZones();

	let saving = $state(false);
	let error = $state<unknown>(null);
	let fields = $state<Record<string, string>>({});

	const recurring = $derived(strategy !== 'single');
	const targetError = $derived(
		Object.entries(fields)
			.filter(([pointer]) => pointer.startsWith('/targets'))
			.map(([, message]) => message)
			.join(' ')
	);

	// Monday first, the way most calendars outside North America read. The
	// values are still 0 = Sunday, matching the API and time.Weekday.
	const WEEK = [1, 2, 3, 4, 5, 6, 0];
	function weekdayName(day: number): string {
		// 1 January 2023 was a Sunday.
		return new Intl.DateTimeFormat(i18n.locale, { weekday: 'short', timeZone: 'UTC' }).format(
			new Date(Date.UTC(2023, 0, 1 + day))
		);
	}

	function toggle<T>(list: T[], value: T): T[] {
		return list.includes(value) ? list.filter((v) => v !== value) : [...list, value];
	}

	function removeMonitor(id: string) {
		picked = picked.filter((m) => m.id !== id);
	}

	/** Starts the window now and ends it after `minutes`: the "we are deploying" path. */
	function startNow(minutes: number) {
		startsAt = nowInZone(timezone);
		endsAt = addMinutes(startsAt, minutes, timezone);
	}

	$effect(() => {
		(async () => {
			// Named one at a time, as the incident form does: a window names a
			// handful of monitors, and broad coverage is what groups and tags are for.
			const ids = existing?.targets.monitor_ids ?? initialMonitorIds;
			for (const id of ids) {
				try {
					const monitor = await api.get<{ id: string; name: string }>(`/monitors/${id}`);
					picked = [...picked, { id: monitor.id, name: monitor.name }];
				} catch {
					// Deleted since. Nothing to show a chip for.
				}
			}
		})();

		const list = async <T,>(scope: string, path: string): Promise<T[]> => {
			if (!session.allows(scope)) return [];
			try {
				return (await api.get<ApiPage<T>>(path)).data;
			} catch {
				// The control hides itself rather than breaking the form.
				return [];
			}
		};
		void list<Group>('groups:read', '/groups?limit=200').then((v) => (groups = v));
		void list<Tag>('tags:read', '/tags?limit=200').then((v) => (tags = v));
		void list<StatusPage>('status_pages:read', '/status-pages?limit=200').then((v) => (pages = v));
	});

	function buildBody(): Record<string, unknown> {
		const body: Record<string, unknown> = {
			title,
			description,
			strategy,
			timezone,
			starts_at: fromZonedInput(startsAt, timezone),
			targets: {
				monitor_ids: picked.map((m) => m.id),
				group_ids: groupIds,
				tag_ids: tagIds
			},
			suppress_notifications: suppress,
			show_on_status_pages: showOnPages
		};

		// An update replaces the window, and a field left out keeps its old
		// value — so the recurrence is always sent whole, and switching from
		// weekly to daily does not leave last week's weekdays behind.
		const untilISO = until ? fromZonedInput(until, timezone) : undefined;
		if (strategy === 'single') {
			body.ends_at = fromZonedInput(endsAt, timezone);
			body.recurrence = {};
		} else {
			body.duration_minutes = durationMinutes;
			const recurrence: Record<string, unknown> = {};
			if (strategy === 'recurring_weekly')
				recurrence.weekdays = [...weekdays].sort((a, b) => a - b);
			if (strategy === 'recurring_monthly')
				recurrence.days_of_month = [...daysOfMonth].sort((a, b) => a - b);
			if (strategy === 'cron') recurrence.cron = cron;
			if (untilISO) recurrence.until = untilISO;
			body.recurrence = recurrence;
		}

		if (pagesMode === 'choose') body.status_page_ids = pageIds;
		else body.auto_status_pages = true;
		return body;
	}

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		saving = true;
		error = null;
		fields = {};

		try {
			const body = buildBody();
			if (editing && existing) {
				await api.patch<MaintenanceWindow>(`/maintenance-windows/${existing.id}`, body);
			} else {
				await api.post<MaintenanceWindow>('/maintenance-windows', body);
			}
			await goto('/maintenance');
		} catch (caught) {
			error = caught;
			if (caught instanceof ApiError) fields = caught.fields();
		} finally {
			saving = false;
		}
	}
</script>

<form onsubmit={submit} class="max-w-2xl space-y-6">
	{#if error}
		<ErrorBox {error} />
	{/if}

	<section class="card space-y-5 p-5">
		<Field
			label={t('maintenance.titleLabel')}
			id="title"
			error={fields['/title']}
			hint={t('maintenance.titleHint')}
		>
			{#snippet children({ id, describedBy, invalid })}
				<input
					{id}
					class="field"
					bind:value={title}
					required
					maxlength="200"
					aria-describedby={describedBy}
					aria-invalid={invalid}
				/>
			{/snippet}
		</Field>

		<Field
			label={t('maintenance.description')}
			id="description"
			optional
			error={fields['/description']}
			hint={t('maintenance.descriptionHint')}
		>
			{#snippet children({ id, describedBy, invalid })}
				<textarea
					{id}
					class="field min-h-20"
					bind:value={description}
					maxlength="5000"
					aria-describedby={describedBy}
					aria-invalid={invalid}
				></textarea>
			{/snippet}
		</Field>
	</section>

	<section class="card space-y-5 p-5">
		<h2 class="font-medium">{t('maintenance.schedule')}</h2>

		<div class="grid gap-5 sm:grid-cols-2">
			<Field label={t('maintenance.strategy')} id="strategy" error={fields['/strategy']}>
				{#snippet children({ id, describedBy, invalid })}
					<select
						{id}
						class="field"
						bind:value={strategy}
						aria-describedby={describedBy}
						aria-invalid={invalid}
					>
						{#each MAINTENANCE_STRATEGIES as value (value)}
							<option {value}>{t(`maintenance.strategy.${value}`)}</option>
						{/each}
					</select>
				{/snippet}
			</Field>

			<Field
				label={t('maintenance.timezone')}
				id="timezone"
				error={fields['/timezone']}
				hint={t('maintenance.timezoneHint')}
			>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						class="field"
						bind:value={timezone}
						list="maintenance-zones"
						required
						autocomplete="off"
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
					<datalist id="maintenance-zones">
						{#each zones as name (name)}
							<option value={name}></option>
						{/each}
					</datalist>
				{/snippet}
			</Field>
		</div>

		{#if !recurring}
			<div class="flex flex-wrap items-center gap-2">
				<span class="muted text-sm">{t('maintenance.startNow')}</span>
				{#each [30, 60, 120, 240] as minutes (minutes)}
					<Button size="sm" onclick={() => startNow(minutes)}>
						{minutes < 60
							? t('maintenance.forMinutes', { n: minutes })
							: t('maintenance.forHours', { count: minutes / 60 })}
					</Button>
				{/each}
			</div>
		{/if}

		<div class="grid gap-5 sm:grid-cols-2">
			<Field
				label={recurring ? t('maintenance.firstStartsAt') : t('maintenance.startsAt')}
				id="starts"
				error={fields['/starts_at']}
				hint={recurring ? t('maintenance.firstStartsAtHint') : undefined}
			>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						type="datetime-local"
						class="field"
						bind:value={startsAt}
						required
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>

			{#if recurring}
				<Field
					label={t('maintenance.durationMinutes')}
					id="duration"
					error={fields['/duration_minutes']}
					hint={t('maintenance.durationHint')}
				>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							type="number"
							min="1"
							max="43200"
							class="field"
							bind:value={durationMinutes}
							required
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
			{:else}
				<Field label={t('maintenance.endsAt')} id="ends" error={fields['/ends_at']}>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							type="datetime-local"
							class="field"
							bind:value={endsAt}
							required
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
			{/if}
		</div>

		{#if strategy === 'recurring_weekly'}
			<fieldset class="space-y-2">
				<legend class="text-sm font-medium">{t('maintenance.weekdays')}</legend>
				<div class="flex flex-wrap gap-2">
					{#each WEEK as day (day)}
						<label
							class="flex cursor-pointer items-center gap-1.5 rounded-md border px-2.5 py-1.5 text-sm"
							style="border-color: {weekdays.includes(day) ? 'var(--accent)' : 'var(--border)'}"
						>
							<input
								type="checkbox"
								class="h-4 w-4 accent-[var(--accent)]"
								checked={weekdays.includes(day)}
								onchange={() => (weekdays = toggle(weekdays, day))}
							/>
							{weekdayName(day)}
						</label>
					{/each}
				</div>
			</fieldset>
		{:else if strategy === 'recurring_monthly'}
			<fieldset class="space-y-2">
				<legend class="text-sm font-medium">{t('maintenance.daysOfMonth')}</legend>
				<p class="muted text-xs">{t('maintenance.daysOfMonthHint')}</p>
				<div class="grid grid-cols-7 gap-1.5 sm:max-w-sm">
					{#each Array.from({ length: 31 }, (_, i) => i + 1) as day (day)}
						<button
							type="button"
							class="rounded-md border py-1.5 text-sm tabular-nums"
							style={daysOfMonth.includes(day)
								? 'border-color: var(--accent); background-color: var(--surface-hover); font-weight: 600'
								: 'border-color: var(--border)'}
							aria-pressed={daysOfMonth.includes(day)}
							onclick={() => (daysOfMonth = toggle(daysOfMonth, day))}
						>
							{day}
						</button>
					{/each}
				</div>
			</fieldset>
		{:else if strategy === 'cron'}
			<Field
				label={t('maintenance.cron')}
				id="cron"
				error={fields['/recurrence']}
				hint={t('maintenance.cronHint')}
			>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						class="field font-mono"
						bind:value={cron}
						required
						placeholder="0 2 * * 0"
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>
		{/if}

		{#if fields['/recurrence'] && strategy !== 'cron'}
			<p class="text-sm" style="color: var(--color-down)">{fields['/recurrence']}</p>
		{/if}

		{#if recurring}
			<Field label={t('maintenance.until')} id="until" optional hint={t('maintenance.untilHint')}>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						type="datetime-local"
						class="field sm:max-w-xs"
						bind:value={until}
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>
		{/if}
	</section>

	<section class="card space-y-5 p-5">
		<div>
			<h2 class="font-medium">{t('maintenance.covers')}</h2>
			<p class="muted mt-1 text-sm">{t('maintenance.coversHint')}</p>
			{#if targetError}
				<p class="mt-2 text-sm" style="color: var(--color-down)">{targetError}</p>
			{/if}
		</div>

		<div class="space-y-2">
			<h3 class="text-sm font-medium">{t('maintenance.monitors')}</h3>
			{#if picked.length}
				<ul class="flex flex-wrap gap-2">
					{#each picked as monitor (monitor.id)}
						<li
							class="flex items-center gap-1.5 rounded-md px-2 py-1 text-sm"
							style="background-color: var(--surface-sunken)"
						>
							<span>{monitor.name}</span>
							<button
								type="button"
								class="muted hover:text-[var(--text)]"
								aria-label={`${t('common.remove')} ${monitor.name}`}
								onclick={() => removeMonitor(monitor.id)}
							>
								&times;
							</button>
						</li>
					{/each}
				</ul>
			{/if}
			<MonitorPicker
				id="maintenance-monitors"
				exclude={chosen}
				onpick={(monitor) => (picked = [...picked, monitor])}
			/>
		</div>

		{#if groups.length}
			<fieldset class="space-y-2">
				<legend class="text-sm font-medium">{t('maintenance.groups')}</legend>
				{#each groups as group (group.id)}
					<label class="flex items-center gap-2 text-sm">
						<input
							type="checkbox"
							class="h-4 w-4 accent-[var(--accent)]"
							checked={groupIds.includes(group.id)}
							onchange={() => (groupIds = toggle(groupIds, group.id))}
						/>
						<span>{group.name}</span>
						<span class="muted text-xs">
							{t('maintenance.monitorCount', { count: group.monitor_count })}
						</span>
					</label>
				{/each}
			</fieldset>
		{/if}

		{#if tags.length}
			<fieldset class="space-y-2">
				<legend class="text-sm font-medium">{t('maintenance.tags')}</legend>
				<p class="muted text-xs">{t('maintenance.tagsHint')}</p>
				<div class="flex flex-wrap gap-2">
					{#each tags as tag (tag.id)}
						<label
							class="flex cursor-pointer items-center gap-1.5 rounded-md border px-2.5 py-1.5 text-sm"
							style="border-color: {tagIds.includes(tag.id) ? 'var(--accent)' : 'var(--border)'}"
						>
							<input
								type="checkbox"
								class="h-4 w-4 accent-[var(--accent)]"
								checked={tagIds.includes(tag.id)}
								onchange={() => (tagIds = toggle(tagIds, tag.id))}
							/>
							{#if tag.color}
								<span
									class="h-2.5 w-2.5 rounded-full"
									style="background-color: {tag.color}"
									aria-hidden="true"
								></span>
							{/if}
							{tag.name}
						</label>
					{/each}
				</div>
			</fieldset>
		{/if}
	</section>

	<section class="card space-y-4 p-5">
		<h2 class="font-medium">{t('maintenance.behaviour')}</h2>
		<label class="flex items-start gap-2 text-sm">
			<input
				type="checkbox"
				class="mt-0.5 h-4 w-4 accent-[var(--accent)]"
				bind:checked={suppress}
			/>
			<span>
				{t('maintenance.suppress')}
				<span class="muted block text-xs">{t('maintenance.suppressHint')}</span>
			</span>
		</label>
		<label class="flex items-start gap-2 text-sm">
			<input
				type="checkbox"
				class="mt-0.5 h-4 w-4 accent-[var(--accent)]"
				bind:checked={showOnPages}
			/>
			<span>
				{t('maintenance.showOnPages')}
				<span class="muted block text-xs">{t('maintenance.showOnPagesHint')}</span>
			</span>
		</label>

		{#if showOnPages && pages.length}
			<div class="space-y-3 border-t pt-4" style="border-color: var(--border)">
				<label class="flex items-start gap-2 text-sm">
					<input
						type="radio"
						name="pages-mode"
						value="auto"
						class="mt-0.5 h-4 w-4 accent-[var(--accent)]"
						bind:group={pagesMode}
					/>
					<span>
						{editing ? t('maintenance.pagesAutoAgain') : t('maintenance.pagesAuto')}
						<span class="muted block text-xs">{t('maintenance.pagesAutoHint')}</span>
					</span>
				</label>
				<label class="flex items-start gap-2 text-sm">
					<input
						type="radio"
						name="pages-mode"
						value="choose"
						class="mt-0.5 h-4 w-4 accent-[var(--accent)]"
						bind:group={pagesMode}
					/>
					<span>{t('maintenance.pagesChoose')}</span>
				</label>

				{#if pagesMode === 'choose'}
					<div class="space-y-2 pl-6">
						{#each pages as page (page.id)}
							<label class="flex items-center gap-2 text-sm">
								<input
									type="checkbox"
									class="h-4 w-4 accent-[var(--accent)]"
									checked={pageIds.includes(page.id)}
									onchange={() => (pageIds = toggle(pageIds, page.id))}
								/>
								<span>{page.title}</span>
								<span class="muted text-xs">/status/{page.slug}</span>
							</label>
						{/each}
						{#if fields['/status_page_ids']}
							<p class="text-sm" style="color: var(--color-down)">
								{fields['/status_page_ids']}
							</p>
						{/if}
					</div>
				{/if}
			</div>
		{/if}
	</section>

	<div class="flex gap-2">
		<Button type="submit" variant="primary" loading={saving} disabled={saving}>
			{editing ? t('common.save') : t('maintenance.create')}
		</Button>
		<Button href="/maintenance">{t('common.cancel')}</Button>
	</div>
</form>
