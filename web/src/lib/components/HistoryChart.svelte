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
	let {
		buckets,
		from = undefined,
		to = undefined,
		height = 160
	}: { buckets: HistoryBucket[]; from?: string; to?: string; height?: number } = $props();

	const width = 900;
	const padding = { top: 8, right: 8, bottom: 22, left: 44 };
	const plotWidth = width - padding.left - padding.right;
	// Derived rather than computed once: `height` is a prop, and a caller that
	// changes it would otherwise keep the first render's geometry.
	const plotHeight = $derived(height - padding.top - padding.bottom);

	/**
	 * The averages, which are what the line is drawn from.
	 *
	 * Deliberately *not* what the minimum and maximum below are drawn from. A
	 * bucket already carries the extremes of the checks inside it, and reducing it
	 * to its average throws them away — over a window held in a single bucket that
	 * makes the three figures identical, which is how this was noticed.
	 */
	const measured = $derived(
		buckets.map((b) => b.response_time_avg_ms).filter((v): v is number => v !== null)
	);

	/** The y-scale follows the line, which is the averages, not the extremes. */
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

	/** The extremes of every check in the window, read from the buckets' own. */
	function extreme(
		pick: (b: HistoryBucket) => number | null,
		reduce: (values: number[]) => number
	) {
		const values = buckets.map(pick).filter((v): v is number => v !== null);
		// Null is not zero: a window in which nothing was measured has no minimum,
		// and reporting 0 ms would read as an impossibly fast response.
		return values.length ? reduce(values) : null;
	}

	const slowest = $derived(
		extreme(
			(b) => b.response_time_max_ms,
			(v) => Math.max(...v)
		)
	);
	const fastest = $derived(
		extreme(
			(b) => b.response_time_min_ms,
			(v) => Math.min(...v)
		)
	);

	/**
	 * The mean response time across the window, weighted by how many checks each
	 * bucket holds.
	 *
	 * An unweighted mean of bucket averages is only correct when every bucket
	 * holds the same number of checks, which is exactly what a window straddling
	 * the start of monitoring does not do — one bucket with three checks would
	 * count as heavily as one with sixty.
	 *
	 * The weight is the bucket's total check count, and that is an approximation
	 * with a named limit: the schema exposes how many checks a bucket held but not
	 * how many of them produced a timing, and a check that timed out has no
	 * response time to contribute. Where every check yields a timing — the ordinary
	 * case for a monitor that is up — the two are the same number. Closing the gap
	 * properly means the response-time count on the wire, which is frozen-spec
	 * surface rather than a frontend decision.
	 */
	const average = $derived.by(() => {
		let total = 0;
		let checks = 0;
		for (const bucket of buckets) {
			if (bucket.response_time_avg_ms === null) continue;
			const weight =
				bucket.up_count + bucket.down_count + bucket.maintenance_count + bucket.pending_count;
			// A bucket that reported an average must have held a check; falling back
			// to 1 keeps a malformed count from erasing it.
			const n = weight > 0 ? weight : 1;
			total += bucket.response_time_avg_ms * n;
			checks += n;
		}
		return checks > 0 ? total / checks : null;
	});
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
				stroke="var(--color-up)"
				stroke-width="2"
				stroke-linejoin="round"
				stroke-linecap="round"
				vector-effect="non-scaling-stroke"
			/>
		{/each}

		{#each isolated as { bucket, index } (index)}
			<circle
				cx={x(index)}
				cy={y(bucket.response_time_avg_ms ?? 0)}
				r="2.5"
				fill="var(--color-up)"
			/>
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

	<!-- The window that was asked for, not the span of the buckets that came back.
	     A window held in a single bucket has one bucket_start, so labelling both
	     ends from the data prints the same timestamp twice and claims the chart
	     covers no time at all. -->
	<div class="muted mt-1 flex justify-between text-xs">
		<span>{formatAbsolute(from ?? buckets[0]?.bucket_start)}</span>
		<span>{formatAbsolute(to ?? buckets[buckets.length - 1]?.bucket_start)}</span>
	</div>

	<div class="mt-4 grid grid-cols-3 gap-3 border-t pt-4" style="border-color: var(--border)">
		{#each [{ label: t('chart.average'), value: average }, { label: t('chart.minimum'), value: fastest }, { label: t('chart.maximum'), value: slowest }] as stat (stat.label)}
			<div>
				<p class="text-lg font-semibold tabular-nums">{formatResponseTime(stat.value)}</p>
				<p class="muted text-xs">{stat.label}</p>
			</div>
		{/each}
	</div>
{/if}
