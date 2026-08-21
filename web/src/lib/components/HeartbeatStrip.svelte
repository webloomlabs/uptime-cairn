<script lang="ts">
	import type { Heartbeat } from '$lib/types';
	import { formatRelative, formatResponseTime, statusLabel } from '$lib/format';
	import { t } from '$lib/i18n/index.svelte';

	/**
	 * The last N checks, oldest to newest.
	 *
	 * Newest on the right, matching every other timeline in the product and the
	 * direction the history chart runs. The API returns heartbeats newest-first,
	 * so they are reversed here once rather than in each caller.
	 *
	 * Padded at the front when there is less history than the strip is wide, so a
	 * monitor created an hour ago has the same width as one running for a year
	 * rather than a short stub that reads as missing data.
	 */
	let {
		beats,
		limit = 30,
		height = 22,
		pad = true
	}: { beats: Heartbeat[]; limit?: number; height?: number; pad?: boolean } = $props();

	const shown = $derived.by(() => {
		const tail = beats.slice(0, limit).reverse();
		if (!pad || tail.length >= limit) return tail;
		return [...(Array(limit - tail.length).fill(null) as (Heartbeat | null)[]), ...tail];
	});

	function tone(beat: Heartbeat | null): string {
		if (!beat) return 'unknown';
		// A suppressed beat is a real observation nobody was paged for. Drawing it
		// as the underlying status would hide that the window did its job; drawing
		// it as up would hide the outage. It gets its own tone.
		if (beat.suppressed) return 'maintenance';
		return beat.status;
	}

	function describe(beat: Heartbeat | null): string {
		if (!beat) return t('public.noData');
		return `${statusLabel(beat.status)} · ${formatRelative(beat.time)} · ${formatResponseTime(
			beat.response_time_ms
		)}${beat.message ? ` · ${beat.message}` : ''}`;
	}
</script>

<div
	class="flex items-stretch gap-[2px]"
	style="height: {height}px"
	role="img"
	aria-label={t('monitor.heartbeats')}
>
	{#each shown as beat, index (index)}
		<span
			class="w-[5px] shrink-0 rounded-[2px]"
			style="background-color: var(--color-{tone(beat)}); opacity: {beat ? 1 : 0.35}"
			title={describe(beat)}
		></span>
	{/each}
</div>
