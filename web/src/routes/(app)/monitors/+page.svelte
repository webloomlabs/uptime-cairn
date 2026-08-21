<script lang="ts">
	import { api } from '$lib/api';
	import type { Group, Page as ApiPage, Tag } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import { untrack } from 'svelte';
	import { SvelteSet } from 'svelte/reactivity';
	import { MonitorList } from '$lib/monitorlist.svelte';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Button from '$lib/components/Button.svelte';
	import MonitorRows from '$lib/components/MonitorRows.svelte';
	import BulkBar from '$lib/components/BulkBar.svelte';

	const list = new MonitorList(50);

	let groups = $state<Group[]>([]);
	let tags = $state<Tag[]>([]);
	// A plain Set is not deeply reactive under runes, so selection uses the
	// reactive one: mutating it in place updates the rows that changed rather
	// than reassigning a new Set and re-rendering the whole page on every click.
	let selected = $state(new SvelteSet<string>());

	const statuses = ['up', 'down', 'pending', 'paused', 'maintenance'];
	const types = $derived(session.info?.monitor_types ?? []);

	// Search is debounced: every keystroke is a server-side query against a
	// 5,000-row table, and the interval floor here is what keeps a fast typist
	// from queueing twenty of them.
	let searchText = $state('');
	let searchTimer: ReturnType<typeof setTimeout> | null = null;

	function onSearch(value: string) {
		searchText = value;
		if (searchTimer) clearTimeout(searchTimer);
		searchTimer = setTimeout(() => void list.apply({ search: value }), 250);
	}

	function toggleIn(values: string[], value: string): string[] {
		return values.includes(value) ? values.filter((v) => v !== value) : [...values, value];
	}

	function toggleSelection(id: string) {
		if (selected.has(id)) selected.delete(id);
		else selected.add(id);
	}

	const allOnPageSelected = $derived(
		list.monitors.length > 0 && list.monitors.every((m) => selected.has(m.id))
	);

	function toggleAll() {
		if (allOnPageSelected) {
			for (const monitor of list.monitors) selected.delete(monitor.id);
		} else {
			for (const monitor of list.monitors) selected.add(monitor.id);
		}
	}

	$effect(() => {
		untrack(() => {
			void list.load();
			list.start();
		});

		(async () => {
			// Groups and tags populate the filter controls. Both are small,
			// bounded collections; neither is a monitor list, so both are fetched
			// whole. A failure hides the filter rather than breaking the page.
			try {
				if (session.allows('groups:read')) {
					groups = (await api.get<ApiPage<Group>>('/groups?limit=200')).data;
				}
				if (session.allows('tags:read')) {
					tags = (await api.get<ApiPage<Tag>>('/tags?limit=200')).data;
				}
			} catch {
				groups = [];
				tags = [];
			}
		})();

		return () => list.stop();
	});
</script>

<div class="space-y-5">
	<div class="flex flex-wrap items-center justify-between gap-3">
		<h1 class="text-2xl font-semibold">{t('monitors.title')}</h1>
		{#if session.allows('monitors:write')}
			<Button href="/monitors/new" variant="primary">{t('monitors.new')}</Button>
		{/if}
	</div>

	<div class="surface space-y-3 rounded-lg p-4">
		<div class="flex flex-wrap gap-3">
			<label class="flex-1" style="min-width: 14rem">
				<span class="sr-only">{t('common.search')}</span>
				<input
					type="search"
					value={searchText}
					oninput={(e) => onSearch(e.currentTarget.value)}
					placeholder={t('monitors.searchPlaceholder')}
					class="w-full rounded-md border px-3 py-2 text-sm"
					style="border-color: var(--border-strong); background-color: var(--surface)"
				/>
			</label>

			<label class="text-sm">
				<span class="sr-only">{t('form.enabled')}</span>
				<select
					class="rounded-md border px-3 py-2 text-sm"
					style="border-color: var(--border-strong); background-color: var(--surface)"
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
			</label>

			{#if groups.length}
				<label class="text-sm">
					<span class="sr-only">{t('form.group')}</span>
					<select
						class="rounded-md border px-3 py-2 text-sm"
						style="border-color: var(--border-strong); background-color: var(--surface)"
						value={list.filters.groupId ?? ''}
						onchange={(e) => list.apply({ groupId: e.currentTarget.value || null })}
					>
						<option value="">{t('form.group')}: {t('common.all')}</option>
						{#each groups as group (group.id)}
							<option value={group.id}>{group.name}</option>
						{/each}
					</select>
				</label>
			{/if}

			{#if tags.length}
				<label class="text-sm">
					<span class="sr-only">{t('form.tags')}</span>
					<select
						class="rounded-md border px-3 py-2 text-sm"
						style="border-color: var(--border-strong); background-color: var(--surface)"
						value={list.filters.tagId ?? ''}
						onchange={(e) => list.apply({ tagId: e.currentTarget.value || null })}
					>
						<option value="">{t('form.tags')}: {t('common.all')}</option>
						{#each tags as tag (tag.id)}
							<option value={tag.id}>{tag.name}</option>
						{/each}
					</select>
				</label>
			{/if}
		</div>

		<div class="flex flex-wrap items-center gap-2">
			{#each statuses as status (status)}
				<button
					type="button"
					class="rounded-full border px-2.5 py-1 text-xs"
					style="border-color: {list.filters.status.includes(status)
						? 'var(--accent)'
						: 'var(--border-strong)'}; background-color: {list.filters.status.includes(status)
						? 'var(--color-' + status + '-soft)'
						: 'transparent'}"
					aria-pressed={list.filters.status.includes(status)}
					onclick={() => list.apply({ status: toggleIn(list.filters.status, status) })}
				>
					{t(`status.${status}`)}
				</button>
			{/each}

			<span class="mx-1" style="border-left: 1px solid var(--border); height: 1rem"></span>

			{#each types as type (type)}
				<button
					type="button"
					class="rounded-full border px-2.5 py-1 text-xs"
					style="border-color: {list.filters.type.includes(type)
						? 'var(--accent)'
						: 'var(--border-strong)'}"
					aria-pressed={list.filters.type.includes(type)}
					onclick={() => list.apply({ type: toggleIn(list.filters.type, type) })}
				>
					{type}
				</button>
			{/each}

			{#if list.filtered}
				<button
					type="button"
					class="ml-auto text-xs underline"
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

	{#if list.stale}
		<!-- ADR-004: filtered-view membership is reconciled, not pushed. The banner
		     is the honest form of that trade — the view is announced as stale
		     rather than reordering itself under the pointer. -->
		<div
			class="flex items-center justify-between gap-3 rounded-md px-4 py-2 text-sm"
			style="background-color: var(--color-pending-soft)"
			role="status"
		>
			<span>{t('monitors.staleView')}</span>
			<Button size="sm" onclick={() => list.load()}>{t('monitors.refreshView')}</Button>
		</div>
	{/if}

	{#if session.allows('monitors:write') && session.can('bulk_operations') && list.monitors.length > 0}
		<div class="flex items-center gap-3 text-sm">
			<label class="flex items-center gap-2">
				<input type="checkbox" class="h-4 w-4" checked={allOnPageSelected} onchange={toggleAll} />
				{t('bulk.selectAll')}
			</label>
			{#if selected.size > 0}
				<span class="muted">{t('monitors.selected', { count: selected.size })}</span>
			{/if}
		</div>
	{/if}

	{#if list.loading}
		<Spinner />
	{:else if list.error}
		<ErrorBox error={list.error} onretry={() => list.load()} />
	{:else if list.monitors.length === 0}
		<div class="surface rounded-lg px-4 py-10 text-center">
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
			ontoggle={toggleSelection}
		/>

		<div class="flex items-center justify-between gap-3">
			<p class="muted text-sm">
				{#if list.total !== null}
					{t('monitors.showing', { count: list.monitors.length, total: list.total })}
				{/if}
			</p>
			{#if list.hasMore}
				<Button onclick={() => list.loadMore()} loading={list.loadingMore}>
					{t('monitors.loadMore')}
				</Button>
			{/if}
		</div>
	{/if}
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
