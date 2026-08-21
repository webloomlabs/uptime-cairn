<script lang="ts">
	import type { HistoryBucket } from '$lib/types';
	import { formatResponseTime, formatAbsolute, formatUptime } from '$lib/format';
	import { t } from '$lib/i18n/index.svelte';

	/**
	 * Response time over a window, with the buckets that lost time marked underneath.
	 *
	 * Hand-drawn SVG rather than a charting library, and the reason is the same one
	 * the Go side gives for writing its own Prometheus exposition: this is a
	 * polyline and an axis, against a dependency with its own render loop and
	 * bundle. It also lets the one rule that matters be enforced directly — a
	 * bucket with no measurement breaks the line rather than interpolating across
	 * it, because a smooth line drawn through an outage is a picture of an outage
	 * that did not happen.
	 */
	let { buckets, height = 160 }: { buckets: HistoryBucket[]; height?: number } = $props();

	const width = 900;
	const padding = { top: 8, right: 8, bottom: 22, left: 44 };
	const plotWidth = width - padding.left - padding.right;
	// Derived rather than computed once: `height` is a prop, and a caller that
	// changes it would otherwise keep the first render's geometry.
	const plotHeight = $derived(height - padding.top - padding.bottom);

	const measured = $derived(
		buckets.map((b) => b.response_time_avg_ms).filter((v): v is number => v !== null)
	);
	const peak = $derived(measured.length ? Math.max(...measured) : 0);
	// A flat zero-height chart for a monitor that always answers in under a
	// millisecond is worse than a scale with headroom, so the ceiling never
	// collapses to the maximum exactly.
	const ceiling = $derived(peak > 0 ? peak * 1.15 : 1);

	function x(index: number): number {
		if (buckets.length <= 1) return padding.left + plotWidth / 2;
		return padding.left + (index / (buckets.length - 1)) * plotWidth;
	}

	function y(value: number): number {
		return padding.top + plotHeight - (value / ceiling) * plotHeight;
	}

	/**
	 * Segments, not one path: a null bucket ends the current run and starts a new
	 * one, so a gap in the data is a gap in the line.
	 */
	const segments = $derived.by(() => {
		const out: string[] = [];
		let current: string[] = [];
		buckets.forEach((bucket, index) => {
			if (bucket.response_time_avg_ms === null) {
				if (current.length > 1) out.push(current.join(' '));
				current = [];
				return;
			}
			current.push(`${x(index).toFixed(2)},${y(bucket.response_time_avg_ms).toFixed(2)}`);
		});
		if (current.length > 1) out.push(current.join(' '));
		return out;
	});

	/** Single measured points with no neighbour to join, which a polyline cannot draw. */
	const isolated = $derived(
		buckets
			.map((bucket, index) => ({ bucket, index }))
			.filter(({ bucket, index }) => {
				if (bucket.response_time_avg_ms === null) return false;
				const before = buckets[index - 1]?.response_time_avg_ms ?? null;
				const after = buckets[index + 1]?.response_time_avg_ms ?? null;
				return before === null && after === null;
			})
	);

	const ticks = $derived([0, ceiling / 2, ceiling]);
</script>

{#if buckets.length === 0}
	<p class="muted py-8 text-sm">{t('monitor.noHeartbeats')}</p>
{:else}
	<svg
		viewBox="0 0 {width} {height}"
		class="w-full"
		role="img"
		aria-label={t('monitor.responseTime')}
		preserveAspectRatio="none"
	>
		{#each ticks as tick (tick)}
			<line
				x1={padding.left}
				x2={width - padding.right}
				y1={y(tick)}
				y2={y(tick)}
				stroke="var(--border)"
				stroke-width="1"
			/>
			<text
				x={padding.left - 6}
				y={y(tick) + 4}
				text-anchor="end"
				font-size="11"
				fill="var(--text-muted)"
			>
				{Math.round(tick)}
			</text>
		{/each}

		{#each segments as points (points)}
			<polyline
				{points}
				fill="none"
				stroke="var(--accent)"
				stroke-width="2"
				stroke-linejoin="round"
				stroke-linecap="round"
				vector-effect="non-scaling-stroke"
			/>
		{/each}

		{#each isolated as { bucket, index } (index)}
			<circle cx={x(index)} cy={y(bucket.response_time_avg_ms ?? 0)} r="2.5" fill="var(--accent)" />
		{/each}

		<!-- Downtime underneath the line rather than on it: an outage is a
		     property of the window, and drawing it as a spike in latency confuses
		     "slow" with "gone". -->
		{#each buckets as bucket, index (index)}
			{#if bucket.down_count > 0}
				<rect
					x={x(index) - 1.5}
					y={height - padding.bottom + 4}
					width="3"
					height="6"
					fill="var(--color-down)"
				>
					<title>{formatAbsolute(bucket.bucket_start)} — {formatUptime(bucket.uptime_ratio)}</title>
				</rect>
			{/if}
		{/each}
	</svg>

	<div class="muted flex justify-between text-xs">
		<span>{formatAbsolute(buckets[0]?.bucket_start)}</span>
		<span>
			{t('monitor.responseTime')}: {formatResponseTime(
				measured.length ? measured.reduce((a, b) => a + b, 0) / measured.length : null
			)}
		</span>
		<span>{formatAbsolute(buckets[buckets.length - 1]?.bucket_start)}</span>
	</div>
{/if}
