<script lang="ts">
	import type { PublicBarEntry } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { formatDate, formatUptime } from '$lib/format';

	/**
	 * The uptime stones on a status page.
	 *
	 * The load-bearing rule: a day with no data is drawn in the "no data" tone,
	 * never in the down tone. The API makes `uptime_ratio` nullable for exactly
	 * this reason — a page that renders "we were not running yet" as a red stone
	 * invents an outage, and every visitor who scrolls back far enough sees one.
	 *
	 * Thresholds rather than a gradient: a stone is a summary, and an operator
	 * reading a wall of them needs three distinguishable states, not ninety
	 * shades. 100% is up, anything that lost time in the day is degraded, and a
	 * day that was mostly down is down.
	 */
	let { entries, days = 90 }: { entries: PublicBarEntry[]; days?: number } = $props();

	// Oldest to newest, left to right, padded at the front so a page younger than
	// its window keeps a constant width rather than growing day by day.
	const shown = $derived.by(() => {
		const tail = entries.slice(-days);
		const padding: (PublicBarEntry | null)[] = Array(Math.max(0, days - tail.length)).fill(null);
		return [...padding, ...tail];
	});

	function tone(entry: PublicBarEntry | null): string {
		if (!entry || entry.uptime_ratio === null) return 'unknown';
		if (entry.status === 'maintenance') return 'maintenance';
		if (entry.uptime_ratio >= 0.9999) return 'up';
		if (entry.uptime_ratio >= 0.95) return 'pending';
		return 'down';
	}

	function description(entry: PublicBarEntry | null): string {
		if (!entry) return t('public.noData');
		if (entry.uptime_ratio === null) return `${formatDate(entry.date)}: ${t('public.noData')}`;
		return `${formatDate(entry.date)}: ${formatUptime(entry.uptime_ratio)}`;
	}
</script>

<div class="flex items-end gap-[2px]" role="img" aria-label={t('monitor.uptime')}>
	{#each shown as entry, index (index)}
		<span
			class="h-7 min-w-[3px] flex-1 rounded-[2px]"
			style="background-color: var(--color-{tone(entry)})"
			title={description(entry)}
		></span>
	{/each}
</div>
