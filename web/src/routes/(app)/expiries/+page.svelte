<script lang="ts">
	import { untrack } from 'svelte';
	import { api } from '$lib/api';
	import type { Page as ApiPage, UpcomingExpiry } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { formatAbsolute } from '$lib/format';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';

	/**
	 * The expiry calendar.
	 *
	 * Two things this screen has to get right, both of which a naive rendering
	 * gets wrong:
	 *
	 *   - **An expired entry is the most urgent row on the page**, and it sorts
	 *     to the top with a negative count. `within_days` bounds the future and
	 *     says nothing about the past, so a 30-day view still shows what lapsed
	 *     last week — filtering it out would leave the screen looking calm on the
	 *     worst possible day.
	 *   - **A stale observation is flagged.** The row is only as good as the last
	 *     time a probe looked, and a calendar built on a month-old observation can
	 *     be confidently wrong. The observation date travels with every row for
	 *     exactly this reason, so it is shown rather than dropped.
	 */
	let entries = $state<UpcomingExpiry[]>([]);
	let loading = $state(true);
	let error = $state<unknown>(null);
	let within = $state<'7' | '30' | '90' | ''>('90');

	async function load() {
		loading = true;
		error = null;
		try {
			const query = within ? `&within_days=${within}` : '';
			entries = (await api.get<ApiPage<UpcomingExpiry>>(`/expiries?limit=200${query}`)).data;
		} catch (caught) {
			error = caught;
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		within;
		untrack(() => void load());
	});

	/**
	 * Tone by urgency, and the thresholds are the ones an operator acts on: a
	 * certificate is usually renewed automatically at 30 days, so inside 14 is
	 * "something did not happen" and past zero is an outage waiting.
	 */
	function tone(days: number): string {
		if (days < 0) return 'down';
		if (days <= 14) return 'pending';
		return 'up';
	}

	function remaining(days: number): string {
		if (days < 0) return `${t('expiries.lapsed')} ${Math.abs(days)}d`;
		return `${days}d`;
	}

	const STALE_DAYS = 7;

	function stale(observed: string): boolean {
		return (Date.now() - Date.parse(observed)) / 86_400_000 > STALE_DAYS;
	}
</script>

<PageTitle title={t('expiries.title')} />

<div class="mb-4 flex flex-wrap items-center gap-2">
	<label class="flex items-center gap-2 text-sm">
		<span class="muted">{t('expiries.within')}</span>
		<select class="field w-auto" bind:value={within}>
			<option value="7">{t('expiries.within.7')}</option>
			<option value="30">{t('expiries.within.30')}</option>
			<option value="90">{t('expiries.within.90')}</option>
			<option value="">{t('expiries.within.all')}</option>
		</select>
	</label>
</div>

{#if loading}
	<Spinner />
{:else if error}
	<ErrorBox {error} onretry={load} />
{:else if entries.length === 0}
	<div class="card px-4 py-14 text-center">
		<p class="font-medium">{t('expiries.empty')}</p>
		<p class="muted mx-auto mt-1 max-w-md text-sm">{t('expiries.emptyHint')}</p>
	</div>
{:else}
	<div class="card overflow-x-auto">
		<table class="w-full text-sm">
			<thead>
				<tr class="border-b text-left" style="border-color: var(--border)">
					{#each [t('expiries.kind'), t('nav.monitoring'), t('expiries.subject'), t('expiries.issuer'), t('expiries.expires'), t('expiries.remaining'), t('expiries.observed')] as heading (heading)}
						<th class="muted px-4 py-3 text-xs font-medium whitespace-nowrap">{heading}</th>
					{/each}
				</tr>
			</thead>
			<tbody>
				{#each entries as entry (entry.kind + entry.monitor_id)}
					<tr class="border-b last:border-0" style="border-color: var(--border)">
						<td class="px-4 py-3.5 whitespace-nowrap">
							<span class="inline-flex items-center gap-1.5">
								<Icon name={entry.kind === 'certificate' ? 'shield' : 'globe'} size={15} />
								{t(`expiries.kind.${entry.kind}`)}
							</span>
						</td>
						<td class="max-w-48 px-4 py-3.5">
							<a href="/monitors/{entry.monitor_id}" class="block truncate hover:underline">
								{entry.monitor_name}
							</a>
						</td>
						<td class="muted max-w-56 truncate px-4 py-3.5" title={entry.subject ?? ''}>
							{entry.subject ?? '—'}
						</td>
						<td class="muted max-w-48 truncate px-4 py-3.5" title={entry.issuer ?? ''}>
							{entry.issuer ?? '—'}
						</td>
						<td class="px-4 py-3.5 whitespace-nowrap">{formatAbsolute(entry.expires_at)}</td>
						<td class="px-4 py-3.5 tabular-nums whitespace-nowrap">
							<span class="font-medium" style="color: var(--color-{tone(entry.days_remaining)})">
								{remaining(entry.days_remaining)}
							</span>
						</td>
						<td class="muted px-4 py-3.5 text-xs whitespace-nowrap">
							{formatAbsolute(entry.observed_at)}
							{#if stale(entry.observed_at)}
								<span
									class="ml-1"
									style="color: var(--color-pending)"
									title={t('expiries.staleHint')}>⚠</span
								>
							{/if}
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
{/if}
