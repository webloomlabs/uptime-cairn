<script lang="ts">
	import type { HistoryBucket } from '$lib/types';
	import { formatAbsolute, formatUptime } from '$lib/format';
	import { t } from '$lib/i18n/index.svelte';

	/**
	 * One stone per bucket across a window, laid on the window's own grid.
	 *
	 * The distinction from HeartbeatStrip, which this replaced on the monitor
	 * page: a strip of the last N checks is a strip of the last N checks, and
	 * labelling it "last 24 hours" is only true for a monitor whose interval
	 * happens to make it so. A monitor on a one-minute interval showed half an
	 * hour under a heading that claimed a day.
	 *
	 * So the geometry comes from the window rather than from the data. Every slot
	 * between `from` and `to` is drawn, and a slot the API returned no bucket for
	 * is drawn as "no data" — because the API omits a bucket that held no checks,
	 * and an hour nobody observed is not an hour that was up.
	 */
	let {
		buckets,
		from,
		to,
		interval,
		height = 26
	}: {
		buckets: HistoryBucket[];
		from: string;
		to: string;
		/** Bucket width in milliseconds; must match the resolution that was asked for. */
		interval: number;
		height?: number;
	} = $props();

	/**
	 * Buckets are aligned to the epoch, not to `from`, so the grid is too —
	 * otherwise every bucket falls between two slots and none of them land.
	 */
	const gridStart = $derived(Math.floor(new Date(from).getTime() / interval) * interval);

	const slots = $derived.by(() => {
		const end = new Date(to).getTime();
		const count = Math.max(1, Math.ceil((end - gridStart) / interval));

		const byStart = new Map<number, HistoryBucket>();
		for (const bucket of buckets) {
			byStart.set(new Date(bucket.bucket_start).getTime(), bucket);
		}

		return Array.from({ length: count }, (_, index) => {
			const start = gridStart + index * interval;
			return { start, bucket: byStart.get(start) ?? null };
		});
	});

	/**
	 * The same three-state reading UptimeBar gives a status page day, for the same
	 * reason: a stone is a summary, and an operator scanning a row of them needs
	 * states they can tell apart, not a gradient.
	 */
	function tone(bucket: HistoryBucket | null): string {
		if (!bucket) return 'unknown';
		if (bucket.uptime_ratio === null) {
			// Nothing counted toward uptime. A maintenance window is the one case
			// where that has an explanation worth drawing.
			return bucket.maintenance_count > 0 ? 'maintenance' : 'unknown';
		}
		if (bucket.uptime_ratio >= 0.9999) return 'up';
		if (bucket.uptime_ratio >= 0.95) return 'pending';
		return 'down';
	}

	function describe(slot: { start: number; bucket: HistoryBucket | null }): string {
		const when = formatAbsolute(new Date(slot.start).toISOString());
		if (!slot.bucket || slot.bucket.uptime_ratio === null) return `${when}: ${t('public.noData')}`;
		return `${when}: ${formatUptime(slot.bucket.uptime_ratio)}`;
	}
</script>

<div
	class="flex items-stretch gap-[2px]"
	style="height: {height}px"
	role="img"
	aria-label={t('monitor.uptime')}
>
	{#each slots as slot (slot.start)}
		<span
			class="min-w-[3px] flex-1 rounded-[2px]"
			style="background-color: var(--color-{tone(slot.bucket)}); opacity: {slot.bucket ? 1 : 0.35}"
			title={describe(slot)}
		></span>
	{/each}
</div>
