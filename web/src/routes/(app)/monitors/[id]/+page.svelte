<script lang="ts">
	import { untrack } from 'svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { api, ApiError } from '$lib/api';
	import type { CertificateInfo, Heartbeat, History, Monitor, Page as ApiPage } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import { specFor } from '$lib/monitortypes';
	import {
		formatAbsolute,
		formatDuration,
		formatRelative,
		formatResponseTime,
		formatUptime,
		monitorTarget,
		statusLabel
	} from '$lib/format';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Button from '$lib/components/Button.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import StatusDial from '$lib/components/StatusDial.svelte';
	import HistoryBar from '$lib/components/HistoryBar.svelte';
	import HistoryChart from '$lib/components/HistoryChart.svelte';

	type UptimeSummary = {
		maintenance_handling: string;
		windows: Record<
			string,
			{
				uptime_ratio: number | null;
				total_checks: number;
				down_checks: number;
				downtime_seconds: number;
				response_time_avg_ms: number | null;
				response_time_p95_ms: number | null;
			}
		>;
	};

	const id = $derived(page.params.id!);

	let monitor = $state<Monitor | null>(null);
	let beats = $state<Heartbeat[]>([]);
	let history = $state<History | null>(null);
	let day = $state<History | null>(null);
	let uptime = $state<UptimeSummary | null>(null);
	let certificate = $state<CertificateInfo | null>(null);
	let error = $state<unknown>(null);
	let busy = $state<string | null>(null);
	let span = $state('24h');

	/**
	 * The preset windows, and they deliberately reach past a month.
	 *
	 * The Phase 1 plan's complaint was a default UI "week-blinkered like Kuma's",
	 * and a picker whose longest option is thirty days answers it only halfway:
	 * retention keeps daily buckets indefinitely, so a year is a question the data
	 * can already answer and the screen simply never asked.
	 */
	const SPANS: Record<string, number> = {
		'1h': 3600_000,
		'24h': 86_400_000,
		'7d': 604_800_000,
		'30d': 2_592_000_000,
		'90d': 7_776_000_000,
		'1y': 31_536_000_000
	};

	/**
	 * The drilldown: an explicit window rather than a distance back from now.
	 *
	 * A preset answers "how has it been lately"; this answers "what happened in
	 * March", which is a different question and the one a relative picker cannot
	 * express at all. Held as `datetime-local` strings because that is what the
	 * inputs read and write, and converted once at the point of use.
	 */
	const CUSTOM = 'custom';
	let customFrom = $state('');
	let customTo = $state('');
	let rangeProblem = $state<string | null>(null);

	/** Local wall-clock, in the shape `datetime-local` wants. */
	function localInput(at: Date): string {
		const pad = (n: number) => String(n).padStart(2, '0');
		return (
			`${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}` +
			`T${pad(at.getHours())}:${pad(at.getMinutes())}`
		);
	}

	/**
	 * The window to ask for, or null when the custom range is not usable yet.
	 *
	 * The refusal is here rather than at the server because an empty second field
	 * is a half-typed range and not an error somebody should be told about — the
	 * chart simply keeps showing what it was showing. A backwards range *is* worth
	 * saying out loud, because it looks like a working control that returns
	 * nothing.
	 */
	function windowFor(): { from: Date; to: Date } | null {
		if (span !== CUSTOM) {
			const to = new Date();
			return { from: new Date(to.getTime() - SPANS[span]), to };
		}
		if (!customFrom || !customTo) return null;
		const from = new Date(customFrom);
		const to = new Date(customTo);
		if (Number.isNaN(from.getTime()) || Number.isNaN(to.getTime())) return null;
		if (from >= to) {
			rangeProblem = t('monitor.rangeBackwards');
			return null;
		}
		rangeProblem = null;
		return { from, to };
	}

	// The uptime bar's own window, fixed at twenty-four hours in one-hour stones
	// so that it says what the heading beside it says. It is a second request
	// rather than a slice of `history`, because that one follows the chart's span
	// picker and the bar must not change shape when somebody looks at an hour.
	const BAR_HOUR = 3600_000;

	async function loadCore() {
		const [loaded, recent] = await Promise.all([
			api.get<Monitor>(`/monitors/${id}`),
			api.get<ApiPage<Heartbeat>>(`/monitors/${id}/heartbeats?limit=60`)
		]);
		monitor = loaded;
		beats = recent.data;
	}

	async function loadHistory() {
		const range = windowFor();
		if (!range) return;
		// `resolution` is left at the server's default of `auto`, which is the
		// whole point: the tier is chosen from the span by the same rule a report
		// follows, and the answer comes back saying which one it picked. Asking for
		// a fixed tier here would produce a chart that is silently empty the moment
		// somebody drills past that tier's retention.
		history = await api.get<History>(
			`/monitors/${id}/history?from=${range.from.toISOString()}&to=${range.to.toISOString()}`
		);
	}

	async function loadDay() {
		const to = new Date();
		const from = new Date(to.getTime() - SPANS['24h']);
		day = await api.get<History>(
			`/monitors/${id}/history?from=${from.toISOString()}&to=${to.toISOString()}&resolution=1h`
		);
	}

	async function loadEverything() {
		error = null;
		try {
			await loadCore();
			await Promise.all([loadHistory(), loadDay()]);

			// Both are supplementary: a monitor page that fails entirely because a
			// certificate has never been observed would be useless for the several
			// monitor types that present none.
			try {
				uptime = await api.get<UptimeSummary>(
					`/monitors/${id}/uptime?window=24h&window=7d&window=30d`
				);
			} catch {
				uptime = null;
			}
			try {
				certificate = session.can('certificate_detail')
					? await api.get<CertificateInfo>(`/monitors/${id}/certificate`)
					: null;
			} catch (caught) {
				certificate = null;
				if (caught instanceof ApiError && caught.status >= 500) throw caught;
			}
		} catch (caught) {
			error = caught;
		}
	}

	async function act(path: string, label: string) {
		busy = label;
		try {
			await api.post(`/monitors/${id}/${path}`);
			await loadCore();
		} catch (caught) {
			error = caught;
		} finally {
			busy = null;
		}
	}

	async function remove() {
		if (!monitor || !confirm(t('monitors.confirmDelete', { name: monitor.name }))) return;
		busy = 'delete';
		try {
			await api.delete(`/monitors/${id}`);
			await goto('/', { replaceState: true });
		} catch (caught) {
			error = caught;
			busy = null;
		}
	}

	$effect(() => {
		// Reading `id` is what re-runs this when the route parameter changes, and it
		// is the only thing this effect should depend on: loadEverything reaches
		// loadHistory, which reads the selected range, and tracking that would
		// duplicate the effect below on every range change.
		void id;
		untrack(() => void loadEverything());

		const timer = setInterval(() => void loadCore().catch(() => {}), 15000);
		return () => clearInterval(timer);
	});

	// The range picker, which reloads only the chart. All three inputs are tracked
	// so that editing either end of a custom range redraws it.
	$effect(() => {
		void span;
		void customFrom;
		void customTo;
		if (untrack(() => monitor)) void loadHistory().catch(() => {});
	});

	/**
	 * Where the data actually begins, when that is later than what was asked for.
	 *
	 * Retention truncates the start of a long window silently: the response echoes
	 * the requested `from` and the buckets simply start later. Drawing that without
	 * saying so is a chart that looks like an outage-free year when it is really a
	 * year the install has not been running for. The comparison is against the
	 * first bucket rather than a stored coverage figure because the history
	 * response carries no `covered_from` — the report document has one, this
	 * endpoint does not, and inventing a field would be an API change.
	 *
	 * A bucket's width of slack, so that a window starting mid-bucket does not
	 * report a truncation that is really just alignment.
	 */
	const coverageBegins = $derived.by(() => {
		if (!history || history.data.length === 0) return null;
		const asked = new Date(history.from).getTime();
		const first = new Date(history.data[0].bucket_start).getTime();
		const second =
			history.data.length > 1 ? new Date(history.data[1].bucket_start).getTime() : first;
		const bucketWidth = Math.max(second - first, 60_000);
		return first - asked > bucketWidth ? history.data[0].bucket_start : null;
	});

	const target = $derived(monitor ? monitorTarget(monitor) : '');
	const canWrite = $derived(session.allows('monitors:write'));
	const linkable = $derived(target.startsWith('http://') || target.startsWith('https://'));

	// The countdown tone, against the same 30-day horizon the overview counts.
	const certTone = $derived.by(() => {
		if (!certificate) return 'unknown';
		if (certificate.days_remaining <= 14) return 'down';
		if (certificate.days_remaining <= 30) return 'pending';
		return 'up';
	});
</script>

{#if error && !monitor}
	<ErrorBox {error} onretry={loadEverything} />
{:else if !monitor}
	<Spinner />
{:else}
	<a
		href="/"
		class="muted mb-5 inline-flex items-center gap-1 rounded-lg px-2.5 py-1.5 text-sm transition-colors hover:bg-[var(--surface-hover)]"
	>
		<Icon name="chevronLeft" size={16} />
		{t('nav.monitoring')}
	</a>

	<div class="mb-7 flex flex-wrap items-start justify-between gap-4">
		<div class="flex min-w-0 items-start gap-4">
			<StatusDial status={monitor.enabled ? monitor.status : 'paused'} size={46} />
			<div class="min-w-0">
				<h1 class="flex flex-wrap items-center gap-2 text-2xl font-bold tracking-tight">
					{monitor.name}
					{#if linkable}
						<a
							href={target}
							target="_blank"
							rel="noopener noreferrer"
							class="muted"
							aria-label={target}
						>
							<Icon name="external" size={16} />
						</a>
					{/if}
				</h1>
				<p class="muted mt-1 text-sm break-all">
					{t('monitor.subtitle', { type: specFor(monitor.type)?.label ?? monitor.type })}
					{#if target}
						{#if linkable}
							<a href={target} target="_blank" rel="noopener noreferrer" class="underline">
								{target}
							</a>
						{:else}{target}{/if}
					{/if}
				</p>
				{#if monitor.description}
					<p class="mt-2 text-sm">{monitor.description}</p>
				{/if}
			</div>
		</div>

		{#if canWrite}
			<div class="flex flex-wrap gap-2">
				{#if session.can('manual_check')}
					<Button loading={busy === 'check'} onclick={() => act('check', 'check')}>
						<Icon name="refresh" size={15} />
						{t('monitors.checkNow')}
					</Button>
				{/if}
				{#if monitor.enabled}
					<Button loading={busy === 'pause'} onclick={() => act('pause', 'pause')}>
						<Icon name="pause" size={15} />
						{t('monitors.pause')}
					</Button>
				{:else}
					<Button loading={busy === 'resume'} onclick={() => act('resume', 'resume')}>
						<Icon name="play" size={15} />
						{t('monitors.resume')}
					</Button>
				{/if}
				<Button href="/monitors/{id}/edit">
					<Icon name="edit" size={15} />
					{t('common.edit')}
				</Button>
				<Button variant="danger" loading={busy === 'delete'} onclick={remove}>
					<Icon name="trash" size={15} />
				</Button>
			</div>
		{/if}
	</div>

	{#if error}
		<div class="mb-5"><ErrorBox {error} /></div>
	{/if}

	<div class="flex flex-col gap-5 xl:flex-row">
		<div class="min-w-0 flex-1 space-y-5">
			<div class="grid gap-4 sm:grid-cols-3">
				<section class="card p-5">
					<h2 class="muted text-sm">{t('monitor.currentStatus')}</h2>
					<p
						class="mt-1 text-2xl font-semibold"
						style="color: var(--color-{monitor.enabled ? monitor.status : 'paused'})"
					>
						{statusLabel(monitor.enabled ? monitor.status : 'paused')}
					</p>
					<p class="muted mt-1 text-xs">
						{monitor.last_check_at
							? t('monitor.since', { when: formatRelative(monitor.last_check_at) })
							: t('monitor.notCheckedYet')}
					</p>
				</section>

				<section class="card p-5">
					<h2 class="muted text-sm">{t('monitors.lastCheck')}</h2>
					<p class="mt-1 text-2xl font-semibold">
						{monitor.last_check_at ? formatRelative(monitor.last_check_at) : '—'}
					</p>
					<p class="muted mt-1 text-xs">
						{t('monitor.everyInterval', { interval: formatDuration(monitor.interval_seconds) })}
					</p>
				</section>

				<section class="card p-5">
					<div class="flex items-baseline justify-between gap-2">
						<h2 class="muted text-sm">{t('overview.last24h')}</h2>
						<span class="text-sm font-semibold tabular-nums">
							{formatUptime(uptime?.windows?.['24h']?.uptime_ratio ?? null)}
						</span>
					</div>
					<div class="mt-3">
						{#if day}
							<HistoryBar
								buckets={day.data}
								from={day.from}
								to={day.to}
								interval={BAR_HOUR}
								height={26}
							/>
						{:else}
							<!-- Nothing rather than the last N checks: a strip of recent
							     beats under this heading is the claim the bar exists to
							     stop making. -->
							<div style="height: 26px"></div>
						{/if}
					</div>
					<p class="muted mt-2 text-xs">
						{t('monitor.downChecks', { count: uptime?.windows?.['24h']?.down_checks ?? 0 })}
					</p>
				</section>
			</div>

			<section class="card grid gap-4 p-5 sm:grid-cols-4">
				{#each ['24h', '7d', '30d'] as window (window)}
					<div>
						<p class="muted text-sm">{t('monitor.lastWindow', { window })}</p>
						<p class="mt-1 text-xl font-semibold tabular-nums" style="color: var(--color-up)">
							{formatUptime(uptime?.windows?.[window]?.uptime_ratio ?? null)}
						</p>
						<p class="muted mt-1 text-xs">
							{t('monitor.downtime', {
								seconds: formatDuration(uptime?.windows?.[window]?.downtime_seconds ?? 0)
							})}
						</p>
					</div>
				{/each}
				<div>
					<p class="muted text-sm">{t('monitor.p95')}</p>
					<p class="mt-1 text-xl font-semibold tabular-nums">
						{formatResponseTime(uptime?.windows?.['24h']?.response_time_p95_ms ?? null)}
					</p>
					<p class="muted mt-1 text-xs">{t('overview.last24h')}</p>
				</div>
			</section>

			<section class="card space-y-3 p-5">
				<div class="flex flex-wrap items-center justify-between gap-3">
					<h2 class="font-semibold">{t('monitor.responseTime')}</h2>
					<div class="flex flex-wrap gap-1">
						{#each Object.keys(SPANS) as option (option)}
							<button
								type="button"
								class="rounded-md px-2.5 py-1 text-xs transition-colors"
								style={span === option
									? 'background-color: var(--surface-hover); font-weight: 600'
									: 'color: var(--text-muted)'}
								aria-pressed={span === option}
								onclick={() => (span = option)}
							>
								{option}
							</button>
						{/each}
						<!--
							The drilldown, beside the presets rather than behind a disclosure.
							A relative picker and an absolute range answer different questions
							— "how has it been lately" against "what happened in March" — and
							hiding the second one is what made the screen week-blinkered.
						-->
						<button
							type="button"
							class="rounded-md px-2.5 py-1 text-xs transition-colors"
							style={span === CUSTOM
								? 'background-color: var(--surface-hover); font-weight: 600'
								: 'color: var(--text-muted)'}
							aria-pressed={span === CUSTOM}
							onclick={() => {
								// Seeded with the window currently on screen, so opening the
								// control starts from what is being looked at rather than from
								// two empty fields.
								if (span !== CUSTOM) {
									const now = new Date();
									customTo = localInput(now);
									customFrom = localInput(new Date(now.getTime() - SPANS[span]));
								}
								span = CUSTOM;
							}}
						>
							{t('monitor.rangeCustom')}
						</button>
					</div>
				</div>

				{#if span === CUSTOM}
					<div class="flex flex-wrap items-end gap-3">
						<label class="text-xs">
							<span class="muted mb-1 block">{t('monitor.rangeFrom')}</span>
							<input type="datetime-local" class="field text-sm" bind:value={customFrom} />
						</label>
						<label class="text-xs">
							<span class="muted mb-1 block">{t('monitor.rangeTo')}</span>
							<input type="datetime-local" class="field text-sm" bind:value={customTo} />
						</label>
					</div>
					{#if rangeProblem}
						<p class="text-xs" style="color: var(--color-down)">{rangeProblem}</p>
					{/if}
				{/if}

				{#if history}
					<HistoryChart buckets={history.data} from={history.from} to={history.to} />

					<!--
						**The resolution actually used, on every chart rather than only on the
						drilled-into ones.**

						This is the half of the deliverable that makes the other half honest.
						The tier is chosen from the span, so a year of history is a chart of
						daily buckets and a p95 that no longer exists at that grain — and a
						reader who is not told will compare it against an hourly chart from
						last week as though the two were the same measurement. Retention
						decides what can answer; this says what did.
					-->
					<p class="muted text-xs">
						{t('monitor.resolutionUsed', { resolution: t(`monitor.tier.${history.resolution}`) })}
						{#if coverageBegins}
							· {t('monitor.coverageBegins', { at: formatAbsolute(coverageBegins) })}
						{/if}
					</p>

					<!--
						**An empty range and an untroubled one must not look alike.**

						The tier is chosen from the span, so drilling into a month that is
						past the hourly tier's retention asks for hours that no longer
						exist and draws nothing — while the daily buckets for that same
						month are still there. The chart's own "no heartbeats" reads as a
						fact about the monitor; it is really a fact about the resolution,
						and saying which is the difference between a reader concluding the
						service was never checked and a reader widening the range.
					-->
					{#if history.data.length === 0}
						<p class="text-xs" style="color: var(--color-pending)">
							{t('monitor.emptyAtResolution', {
								resolution: t(`monitor.tier.${history.resolution}`)
							})}
						</p>
					{/if}
				{:else}
					<Spinner />
				{/if}
			</section>

			<section class="card space-y-3 p-5">
				<h2 class="font-semibold">{t('monitor.heartbeats')}</h2>
				{#if beats.length === 0}
					<p class="muted text-sm">{t('monitor.noHeartbeats')}</p>
				{:else}
					<ul class="divide-y text-sm" style="border-color: var(--border)">
						{#each beats.slice(0, 12) as beat (beat.time)}
							<li class="flex flex-wrap items-center gap-x-3 gap-y-1 py-2.5">
								<StatusDial status={beat.suppressed ? 'maintenance' : beat.status} size={18} />
								<span class="muted w-20 tabular-nums" title={formatAbsolute(beat.time)}>
									{formatRelative(beat.time)}
								</span>
								<span class="w-20 tabular-nums">{formatResponseTime(beat.response_time_ms)}</span>
								{#if beat.suppressed}
									<span class="muted text-xs">
										{t('status.maintenance')}{beat.suppression_reason
											? ` (${beat.suppression_reason})`
											: ''}
									</span>
								{/if}
								{#if beat.message}
									<span class="muted min-w-0 flex-1 truncate" title={beat.message}>
										{beat.message}
									</span>
								{/if}
							</li>
						{/each}
					</ul>
				{/if}
			</section>
		</div>

		<aside class="w-full shrink-0 space-y-4 xl:w-72">
			<section class="card p-5">
				<h2 class="font-semibold">
					{t('monitor.certificate')}<span style="color: var(--color-up)" aria-hidden="true">.</span>
				</h2>

				{#if certificate}
					<p class="muted mt-4 text-xs">{t('monitor.certificateValidUntil')}</p>
					<p class="mt-1 flex items-center gap-2 text-sm font-medium">
						<span
							class="inline-block h-2 w-2 rounded-full"
							style="background-color: var(--color-{certTone})"
							aria-hidden="true"
						></span>
						{formatAbsolute(certificate.valid_to)}
					</p>
					<p class="muted mt-1 text-xs">
						{t('monitor.certificateExpires', { when: formatRelative(certificate.valid_to) })}
					</p>

					{#if certificate.issuer}
						<p class="muted mt-4 text-xs">{t('monitor.certificateIssuer')}</p>
						<p class="mt-1 text-sm break-all">{certificate.issuer}</p>
					{/if}

					{#if certificate.chain_valid !== undefined}
						<p class="muted mt-4 text-xs">{t('monitor.certificateChain')}</p>
						<p class="mt-1 text-sm">
							{certificate.chain_valid
								? t('monitor.certificateChainValid')
								: (certificate.chain_error ?? t('common.no'))}
						</p>
					{/if}

					<!-- observed_at means "last confirmed on the wire", to within an hour:
					     the probe resends an unchanged observation hourly rather than on
					     every check (protocol §7.4). Saying so is the difference between a
					     stale-looking timestamp and a wrong one. -->
					<p class="muted mt-4 text-xs">
						{t('monitor.certificateObserved', { when: formatRelative(certificate.observed_at) })}
					</p>
				{:else}
					<p class="muted mt-3 text-sm">{t('monitor.certificateNone')}</p>
				{/if}
			</section>

			<section class="card p-5">
				<h2 class="font-semibold">
					{t('monitor.configuration')}<span style="color: var(--color-up)" aria-hidden="true"
						>.</span
					>
				</h2>
				<dl class="mt-4 space-y-2.5 text-sm">
					{#each [{ k: t('form.interval'), v: formatDuration(monitor.interval_seconds) }, { k: t('form.timeout'), v: formatDuration(monitor.timeout_seconds) }, { k: t('form.retries'), v: String(monitor.retries) }, { k: t('monitors.nextCheck'), v: formatRelative(monitor.next_check_at) }] as row (row.k)}
						<div class="flex items-baseline justify-between gap-3">
							<dt class="muted text-xs">{row.k}</dt>
							<dd class="tabular-nums">{row.v}</dd>
						</div>
					{/each}
					{#if monitor.upside_down}
						<div class="flex items-baseline justify-between gap-3">
							<dt class="muted text-xs">{t('form.upsideDown')}</dt>
							<dd>{t('common.yes')}</dd>
						</div>
					{/if}
				</dl>
			</section>
		</aside>
	</div>
{/if}
