<script lang="ts">
	import { untrack } from 'svelte';
	import { api } from '$lib/api';
	import type {
		MaintenanceState,
		MaintenanceWindow,
		Page as ApiPage,
		StatusPage
	} from '$lib/types';
	import { i18n, t } from '$lib/i18n/index.svelte';
	import { formatAbsolute } from '$lib/format';
	import { session } from '$lib/session.svelte';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Button from '$lib/components/Button.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';

	let windows = $state<MaintenanceWindow[]>([]);
	let pageTitles = $state<Record<string, string>>({});
	let loading = $state(true);
	let error = $state<unknown>(null);
	let search = $state('');
	let filter = $state<'all' | MaintenanceState>('all');
	let ending = $state<string | null>(null);

	const canWrite = $derived(session.allows('maintenance:write'));

	async function load() {
		loading = true;
		error = null;
		try {
			windows = (await api.get<ApiPage<MaintenanceWindow>>('/maintenance-windows?limit=200')).data;
		} catch (caught) {
			error = caught;
		} finally {
			loading = false;
		}

		if (session.allows('status_pages:read')) {
			try {
				const pages = (await api.get<ApiPage<StatusPage>>('/status-pages?limit=200')).data;
				pageTitles = Object.fromEntries(pages.map((p) => [p.id, p.title]));
			} catch {
				// Counts instead of names. Not worth failing the list over.
			}
		}
	}

	$effect(() => {
		untrack(() => void load());
	});

	// Filtered in the client, as incidents are: windows are planned by people,
	// so there are tens of them rather than thousands.
	const ORDER: Record<string, number> = { active: 0, scheduled: 1, ended: 2, cancelled: 3 };
	const shown = $derived(
		windows
			.filter((w) => filter === 'all' || w.state === filter)
			.filter((w) => !search || w.title.toLowerCase().includes(search.toLowerCase()))
			.sort(
				(a, b) =>
					(ORDER[a.state] ?? 9) - (ORDER[b.state] ?? 9) ||
					Date.parse(nextStart(a)) - Date.parse(nextStart(b))
			)
	);
	const activeCount = $derived(windows.filter((w) => w.state === 'active').length);

	/** The occurrence running now or coming next, as the sweep last saw it. */
	function nextStart(w: MaintenanceWindow): string {
		return w.next_occurrence_at ?? w.starts_at;
	}

	function occurrenceEnd(w: MaintenanceWindow): string | null {
		if (w.strategy === 'single' && w.ends_at) return w.ends_at;
		if (!w.duration_minutes) return null;
		return new Date(Date.parse(nextStart(w)) + w.duration_minutes * 60_000).toISOString();
	}

	function weekday(day: number): string {
		return new Intl.DateTimeFormat(i18n.locale, { weekday: 'short', timeZone: 'UTC' }).format(
			new Date(Date.UTC(2023, 0, 1 + day))
		);
	}

	function schedule(w: MaintenanceWindow): string {
		switch (w.strategy) {
			case 'recurring_weekly':
				return t('maintenance.summary.weekly', {
					days: (w.recurrence?.weekdays ?? []).map(weekday).join(', ')
				});
			case 'recurring_monthly':
				return t('maintenance.summary.monthly', {
					days: (w.recurrence?.days_of_month ?? []).join(', ')
				});
			case 'cron':
				return t('maintenance.summary.cron', { cron: w.recurrence?.cron ?? '' });
			default:
				return t(`maintenance.strategy.${w.strategy}`);
		}
	}

	function covers(w: MaintenanceWindow): string {
		const parts: string[] = [];
		const { monitor_ids, group_ids, tag_ids } = w.targets;
		if (monitor_ids.length)
			parts.push(t('maintenance.count.monitors', { count: monitor_ids.length }));
		if (group_ids.length) parts.push(t('maintenance.count.groups', { count: group_ids.length }));
		if (tag_ids.length) parts.push(t('maintenance.count.tags', { count: tag_ids.length }));
		return parts.join(' · ');
	}

	function shownOn(w: MaintenanceWindow): string {
		if (!w.show_on_status_pages || w.status_page_ids.length === 0) return '—';
		const names = w.status_page_ids.map((id) => pageTitles[id]).filter(Boolean);
		return names.length === w.status_page_ids.length
			? names.join(', ')
			: t('maintenance.count.pages', { count: w.status_page_ids.length });
	}

	/**
	 * Ends a running one-off window now, keeping it on record.
	 *
	 * Deleting would also stop it, but would drop the record of when the work
	 * happened. A recurring window has no single end to move, so it is edited or
	 * deleted instead.
	 */
	async function endNow(w: MaintenanceWindow) {
		if (!confirm(t('maintenance.endNowConfirm', { title: w.title }))) return;
		ending = w.id;
		try {
			await api.patch(`/maintenance-windows/${w.id}`, {
				ends_at: new Date().toISOString(),
				status_page_ids: w.status_page_ids
			});
			await load();
		} catch (caught) {
			error = caught;
		} finally {
			ending = null;
		}
	}

	const TONE: Record<string, string> = {
		active: 'maintenance',
		scheduled: 'pending',
		ended: 'paused',
		cancelled: 'paused'
	};
	const FILTERS: ('all' | MaintenanceState)[] = ['all', 'active', 'scheduled', 'ended'];
</script>

<PageTitle title={t('nav.maintenance')}>
	{#snippet actions()}
		{#if canWrite}
			<Button href="/maintenance/new" variant="primary">
				<Icon name="plus" size={16} />
				{t('maintenance.new')}
			</Button>
		{/if}
	{/snippet}
</PageTitle>

{#if activeCount > 0}
	<p
		class="mb-4 flex items-center gap-2 rounded-lg px-4 py-3 text-sm"
		style="background-color: var(--color-maintenance-soft)"
	>
		<Icon name="maintenance" size={16} />
		{t('maintenance.activeBanner', { count: activeCount })}
	</p>
{/if}

<div class="mb-4 flex flex-wrap items-center gap-2">
	<label class="relative min-w-48 flex-1">
		<span class="sr-only">{t('common.search')}</span>
		<span class="muted pointer-events-none absolute top-2.5 left-3">
			<Icon name="search" size={16} />
		</span>
		<input type="search" class="field pl-9" bind:value={search} placeholder={t('common.search')} />
	</label>
	{#each FILTERS as value (value)}
		<button
			type="button"
			class="card shrink-0 px-3 py-2 text-sm"
			style="border-radius: 0.5rem; {filter === value ? 'border-color: var(--accent)' : ''}"
			aria-pressed={filter === value}
			onclick={() => (filter = value)}
		>
			{value === 'all' ? t('common.all') : t(`maintenance.state.${value}`)}
		</button>
	{/each}
</div>

{#if loading}
	<Spinner />
{:else if error}
	<ErrorBox {error} onretry={load} />
{:else if shown.length === 0}
	<div class="card px-4 py-14 text-center">
		<p class="font-medium">{t('maintenance.empty')}</p>
		<p class="muted mt-1 text-sm">{t('maintenance.emptyHint')}</p>
		{#if canWrite}
			<div class="mt-5">
				<Button href="/maintenance/new" variant="primary">{t('maintenance.new')}</Button>
			</div>
		{/if}
	</div>
{:else}
	<div class="card overflow-x-auto">
		<table class="w-full text-sm">
			<thead>
				<tr class="border-b text-left" style="border-color: var(--border)">
					{#each [t('maintenance.col.state'), t('maintenance.col.title'), t('maintenance.col.schedule'), t('maintenance.col.when'), t('maintenance.col.covers'), t('maintenance.col.pages'), ''] as heading, i (i)}
						<th class="muted px-4 py-3 text-xs font-medium whitespace-nowrap">{heading}</th>
					{/each}
				</tr>
			</thead>
			<tbody>
				{#each shown as w (w.id)}
					{@const end = occurrenceEnd(w)}
					<tr class="border-b last:border-0" style="border-color: var(--border)">
						<td class="px-4 py-3.5 whitespace-nowrap">
							<span
								class="rounded px-2 py-0.5 text-xs font-medium"
								style="background-color: var(--color-{TONE[w.state] ?? 'unknown'}-soft)"
							>
								{t(`maintenance.state.${w.state}`)}
							</span>
						</td>
						<td class="max-w-xs px-4 py-3.5">
							<a href="/maintenance/{w.id}" class="block truncate hover:underline" title={w.title}>
								{w.title}
							</a>
							{#if !w.suppress_notifications}
								<span class="muted text-xs">{t('maintenance.alertsStillSent')}</span>
							{/if}
						</td>
						<td class="muted px-4 py-3.5 whitespace-nowrap">{schedule(w)}</td>
						<td class="muted px-4 py-3.5 whitespace-nowrap">
							{#if w.state === 'active'}
								{end ? t('maintenance.runsUntil', { at: formatAbsolute(end) }) : '—'}
							{:else if w.state === 'scheduled'}
								{t('maintenance.starts', { at: formatAbsolute(nextStart(w)) })}
							{:else}
								{end ? t('maintenance.endedAt', { at: formatAbsolute(end) }) : '—'}
							{/if}
						</td>
						<td class="muted px-4 py-3.5 whitespace-nowrap">{covers(w)}</td>
						<td class="muted max-w-48 truncate px-4 py-3.5" title={shownOn(w)}>{shownOn(w)}</td>
						<td class="px-4 py-3.5 text-right whitespace-nowrap">
							{#if canWrite && w.state === 'active' && w.strategy === 'single'}
								<Button
									size="sm"
									loading={ending === w.id}
									disabled={ending !== null}
									onclick={() => endNow(w)}
								>
									{t('maintenance.endNow')}
								</Button>
							{/if}
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
{/if}
