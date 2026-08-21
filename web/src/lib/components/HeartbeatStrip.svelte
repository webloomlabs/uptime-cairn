<script lang="ts">
	import type { Heartbeat } from '$lib/types';
	import { formatRelative, formatResponseTime, statusLabel } from '$lib/format';

	/**
	 * The last N checks, oldest to newest.
	 *
	 * Newest on the right, matching every other timeline in the product and the
	 * direction the history chart runs. The API returns heartbeats newest-first,
	 * so they are reversed here once rather than in each caller.
	 */
	let { beats, limit = 40 }: { beats: Heartbeat[]; limit?: number } = $props();

	const shown = $derived(beats.slice(0, limit).reverse());

	function tone(beat: Heartbeat): string {
		// A suppressed beat is a real observation nobody was paged for. Drawing it
		// as the underlying status would hide that the window did its job; drawing
		// it as up would hide the outage. It gets its own tone.
		if (beat.suppressed) return 'maintenance';
		return beat.status;
	}
</script>

<div class="flex items-end gap-[2px]">
	{#each shown as beat (beat.time)}
		<span
			class="h-6 w-[4px] shrink-0 rounded-[1px]"
			style="background-color: var(--color-{tone(beat)})"
			title="{statusLabel(beat.status)} · {formatRelative(beat.time)} · {formatResponseTime(
				beat.response_time_ms
			)}{beat.message ? ` · ${beat.message}` : ''}"
		></span>
	{/each}
</div>
