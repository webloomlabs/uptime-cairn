<script lang="ts">
	import { untrack } from 'svelte';
	import { SvelteSet } from 'svelte/reactivity';
	import { api } from '$lib/api';
	import type { Group, Overview, Page as ApiPage, Tag } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import { MonitorList } from '$lib/monitorlist.svelte';
	import { formatUptime } from '$lib/format';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Button from '$lib/components/Button.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import StatusDial from '$lib/components/StatusDial.svelte';
	import MonitorRows from '$lib/components/MonitorRows.svelte';
	import BulkBar from '$lib/components/BulkBar.svelte';

	/**
	 * Monitoring: the list, and the summary beside it.
	 *
	 * The headline counts come from `/overview`, computed server-side. Summing
	 * whatever the client happens to have loaded would couple a global number to
	 * viewport state, which ADR-004 rules out explicitly — and it is the reason
	 * the rail can say "18 up" while the list is showing a filtered page of three.
	 */
	const list = new MonitorList(50);

	let overview = $state<Overview | null>(null);
	let overviewError = $state<unknown>(null);
	let groups = $state<Group[]>([]);
	let tags = $state<Tag[]>([]);
	let selected = $state(new SvelteSet<string>());
	let showFilters = $state(false);

	const statuses = ['up', 'down', 'pending', 'paused', 'maintenance'];
	const types = $derived(session.info?.monitor_types ?? []);

	let searchText = $state('');
	let searchTimer: ReturnType<typeof setTimeout> | null = null;

	// Debounced: every keystroke is a server-side query against a table that may
	// hold 5,000 rows, and the delay is what keeps a fast typist from queueing
	// twenty of them.
	function onSearch(value: string) {
		searchText = value;
		if (searchTimer) clearTimeout(searchTimer);
		searchTimer = setTimeout(() => void list.apply({ search: value }), 250);
	}

	async function loadOverview() {
		try {
			overview = await api.get<Overview>('/overview');
			overviewError = null;
		} catch (caught) {
			overviewError = caught;
		}
	}

	function toggleIn(values: string[], value: string): string[] {
		return values.includes(value) ? values.filter((v) => v !== value) : [...values, value];
	}

	const allOnPageSelected = $derived(
		list.monitors.length > 0 && list.monitors.every((m) => selected.has(m.id))
	);

	function toggleAll() {
		if (allOnPageSelected) for (const m of list.monitors) selected.delete(m.id);
		else for (const m of list.monitors) selected.add(m.id);
	}

	// untrack, because this is an imperative bootstrap rather than a computation:
	// the calls inside read and write the very state they manage, and tracking any
	// of it turns mount into a loop.
	$effect(() => {
		untrack(() => {
			void loadOverview();
			void list.load();
			list.start();
		});

		const timer = setInterval(loadOverview, 15000);

		(async () => {
			// Both are small bounded collections and neither is a monitor list, so
			// both are fetched whole. A failure hides the control rather than
			// breaking the page.
			try {
				if (session.allows('groups:read'))
					groups = (await api.get<ApiPage<Group>>('/groups?limit=200')).data;
				if (session.allows('tags:read'))
					tags = (await api.get<ApiPage<Tag>>('/tags?limit=200')).data;
			} catch {
				groups = [];
				tags = [];
			}
		})();

		return () => {
			list.stop();
			clearInterval(timer);
		};
	});

	const overall = $derived.by(() => {
		if (!overview) return 'unknown';
		if (overview.monitors.down > 0) return 'down';
		if (overview.monitors.maintenance > 0) return 'maintenance';
		if (overview.monitors.pending > 0) return 'pending';
		return 'up';
	});
</script>

<PageTitle title={t('nav.monitors')}>
	{#snippet actions()}
		{#if session.allows('monitors:write')}
			<Button href="/monitors/new" variant="primary">
				<Icon name="plus" size={16} />
				{t('common.new')}
			</Button>
		{/if}
	{/snippet}
</PageTitle>

<div class="flex flex-col gap-6 xl:flex-row">
	<div class="min-w-0 flex-1 space-y-4">
		<!-- Toolbar -->
		<div class="flex flex-wrap items-center gap-2">
			{#if session.allows('monitors:write') && session.can('bulk_operations')}
				<label
					class="card flex shrink-0 items-center gap-2 px-3 py-2 text-sm"
					style="border-radius: 0.5rem"
				>
					<input
						type="checkbox"
						class="h-4 w-4 accent-[var(--accent)]"
						checked={allOnPageSelected}
						onchange={toggleAll}
					/>
					<span class="muted tabular-nums">
						{selected.size} / {list.total ?? list.monitors.length}
					</span>
				</label>
			{/if}

			<label class="relative min-w-48 flex-1">
				<span class="sr-only">{t('common.search')}</span>
				<span class="muted pointer-events-none absolute top-2.5 left-3">
					<Icon name="search" size={16} />
				</span>
				<input
					type="search"
					class="field pl-9"
					value={searchText}
					oninput={(e) => onSearch(e.currentTarget.value)}
					placeholder={t('monitors.searchPlaceholder')}
				/>
			</label>

			<button
				type="button"
				class="card flex shrink-0 items-center gap-2 px-3 py-2 text-sm"
				style="border-radius: 0.5rem"
				aria-expanded={showFilters}
				onclick={() => (showFilters = !showFilters)}
			>
				<Icon name="filter" size={16} />
				{t('common.filter')}
				{#if list.filtered}
					<span
						class="rounded-full px-1.5 text-[11px] font-semibold"
						style="background-color: var(--accent); color: var(--accent-contrast)"
					>
						•
					</span>
				{/if}
			</button>
		</div>

		{#if showFilters}
			<div class="card space-y-3 p-4">
				<div class="flex flex-wrap items-center gap-2">
					{#each statuses as status (status)}
						{@const on = list.filters.status.includes(status)}
						<button
							type="button"
							class="rounded-full border px-3 py-1 text-xs transition-colors"
							style="border-color: {on
								? 'var(--color-' + status + ')'
								: 'var(--border-strong)'}; background-color: {on
								? 'var(--color-' + status + '-soft)'
								: 'transparent'}"
							aria-pressed={on}
							onclick={() => list.apply({ status: toggleIn(list.filters.status, status) })}
						>
							{t(`status.${status}`)}
						</button>
					{/each}
				</div>

				<div class="flex flex-wrap items-center gap-2">
					{#each types as type (type)}
						{@const on = list.filters.type.includes(type)}
						<button
							type="button"
							class="rounded-full border px-3 py-1 text-xs transition-colors"
							style="border-color: {on ? 'var(--accent)' : 'var(--border-strong)'}"
							aria-pressed={on}
							onclick={() => list.apply({ type: toggleIn(list.filters.type, type) })}
						>
							{type}
						</button>
					{/each}
				</div>

				<div class="flex flex-wrap gap-2">
					<select
						class="field w-auto"
						value={list.filters.enabled === null ? '' : String(list.filters.enabled)}
						onchange={(e) =>
							list.apply({
								enabled: e.currentTarget.value === '' ? null : e.currentTarget.value === 'true'
							})}
					>
						<option value="">{t('common.all')}</option>
						<option value="true">{t('form.enabled')}</option>
						<option value="false">{t('status.paused')}</option>
					</select>

					{#if groups.length}
						<select
							class="field w-auto"
							value={list.filters.groupId ?? ''}
							onchange={(e) => list.apply({ groupId: e.currentTarget.value || null })}
						>
							<option value="">{t('form.group')}: {t('common.all')}</option>
							{#each groups as group (group.id)}
								<option value={group.id}>{group.name}</option>
							{/each}
						</select>
					{/if}

					{#if tags.length}
						<select
							class="field w-auto"
							value={list.filters.tagId ?? ''}
							onchange={(e) => list.apply({ tagId: e.currentTarget.value || null })}
						>
							<option value="">{t('form.tags')}: {t('common.all')}</option>
							{#each tags as tag (tag.id)}
								<option value={tag.id}>{tag.name}</option>
							{/each}
						</select>
					{/if}

					{#if list.filtered}
						<button
							type="button"
							class="text-xs underline"
							onclick={() => {
								searchText = '';
								void list.reset();
							}}
						>
							{t('common.clear')}
						</button>
					{/if}
				</div>
			</div>
		{/if}

		{#if list.stale}
			<!-- ADR-004: filtered-view membership is reconciled, not pushed. The
			     banner is the honest form of that trade — the view is announced as
			     stale rather than reordering itself under the pointer. -->
			<div
				class="flex items-center justify-between gap-3 rounded-lg px-4 py-2.5 text-sm"
				style="background-color: var(--color-pending-soft)"
				role="status"
			>
				<span>{t('monitors.staleView')}</span>
				<Button size="sm" onclick={() => list.load()}>{t('monitors.refreshView')}</Button>
			</div>
		{/if}

		{#if list.loading}
			<Spinner />
		{:else if list.error}
			<ErrorBox error={list.error} onretry={() => list.load()} />
		{:else if list.monitors.length === 0}
			<div class="card px-4 py-14 text-center">
				<p class="font-medium">{list.filtered ? t('monitors.noMatches') : t('monitors.empty')}</p>
				{#if !list.filtered}
					<p class="muted mt-1 text-sm">{t('monitors.emptyHint')}</p>
				{/if}
			</div>
		{:else}
			<MonitorRows
				monitors={list.monitors}
				selectable={session.allows('monitors:write') && session.can('bulk_operations')}
				{selected}
				ontoggle={(id) => (selected.has(id) ? selected.delete(id) : selected.add(id))}
			/>

			{#if list.hasMore}
				<div class="flex justify-center">
					<Button onclick={() => list.loadMore()} loading={list.loadingMore}>
						{t('monitors.loadMore')}
					</Button>
				</div>
			{/if}
		{/if}
	</div>

	<!-- The summary rail. -->
	<aside class="w-full shrink-0 space-y-4 xl:w-72">
		<section class="card p-5">
			<h2 class="font-semibold">
				{t('overview.currentStatus')}<span style="color: var(--color-up)" aria-hidden="true">.</span
				>
			</h2>

			{#if overviewError}
				<div class="mt-4"><ErrorBox error={overviewError} onretry={loadOverview} /></div>
			{:else if !overview}
				<Spinner />
			{:else}
				<div class="mt-5 flex justify-center">
					<StatusDial status={overall} size={56} />
				</div>
				<dl class="mt-5 grid grid-cols-3 gap-2 text-center">
					{#each [{ k: 'down', v: overview.monitors.down }, { k: 'up', v: overview.monitors.up }, { k: 'paused', v: overview.monitors.paused }] as cell (cell.k)}
						<div>
							<dd class="text-2xl font-semibold tabular-nums">{cell.v}</dd>
							<dt class="muted text-xs">{t(`status.${cell.k}`)}</dt>
						</div>
					{/each}
				</dl>
				{#if overview.monitors.pending || overview.monitors.maintenance}
					<p class="muted mt-3 text-center text-xs">
						{overview.monitors.pending}
						{t('status.pending')} · {overview.monitors.maintenance}
						{t('status.maintenance')}
					</p>
				{/if}
				<p class="muted mt-4 text-center text-xs">
					{t('overview.usingMonitors', { count: overview.monitors.total })}
				</p>
			{/if}
		</section>

		{#if overview}
			<section class="card p-5">
				<h2 class="font-semibold">
					{t('overview.last24h')}<span style="color: var(--color-up)" aria-hidden="true">.</span>
				</h2>
				<dl class="mt-4 grid grid-cols-2 gap-4">
					<div>
						<dd
							class="text-xl font-semibold tabular-nums"
							style={list.monitors.length ? '' : 'color: var(--text-muted)'}
						>
							{formatUptime(
								list.monitors.length
									? list.monitors.reduce((sum, m) => sum + (m.uptime?.['24h'] ?? 0), 0) /
											list.monitors.filter((m) => m.uptime?.['24h'] != null).length || null
									: null
							)}
						</dd>
						<dt class="muted text-xs">{t('overview.pageUptime')}</dt>
					</div>
					<div>
						<dd class="text-xl font-semibold tabular-nums">{overview.active_incidents}</dd>
						<dt class="muted text-xs">{t('overview.activeIncidents')}</dt>
					</div>
					<div>
						<dd class="text-xl font-semibold tabular-nums">
							{overview.certificates_expiring_soon}
						</dd>
						<dt class="muted text-xs">{t('overview.certificatesExpiring')}</dt>
					</div>
					<div>
						<dd class="text-xl font-semibold tabular-nums">{overview.domains_expiring_soon}</dd>
						<dt class="muted text-xs">{t('overview.domainsExpiring')}</dt>
					</div>
				</dl>
				<p class="muted mt-3 text-xs">{t('overview.expiringWindow')}</p>
			</section>
		{/if}
	</aside>
</div>

{#if selected.size > 0}
	<BulkBar
		ids={[...selected]}
		{tags}
		onclear={() => selected.clear()}
		ondone={() => {
			selected.clear();
			void list.load();
		}}
	/>
{/if}
