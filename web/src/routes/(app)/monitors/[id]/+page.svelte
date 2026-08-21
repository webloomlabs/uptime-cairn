<script lang="ts">
	import { untrack } from 'svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { api, ApiError } from '$lib/api';
	import type { CertificateInfo, Heartbeat, History, Monitor, Page as ApiPage } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
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
	import StatusBadge from '$lib/components/StatusBadge.svelte';
	import HeartbeatStrip from '$lib/components/HeartbeatStrip.svelte';
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
	let uptime = $state<UptimeSummary | null>(null);
	let certificate = $state<CertificateInfo | null>(null);
	let error = $state<unknown>(null);
	let busy = $state<string | null>(null);
	let span = $state('24h');

	const SPANS: Record<string, number> = {
		'1h': 3600_000,
		'24h': 86_400_000,
		'7d': 604_800_000,
		'30d': 2_592_000_000
	};

	async function loadCore() {
		const [loaded, recent] = await Promise.all([
			api.get<Monitor>(`/monitors/${id}`),
			api.get<ApiPage<Heartbeat>>(`/monitors/${id}/heartbeats?limit=60`)
		]);
		monitor = loaded;
		beats = recent.data;
	}

	async function loadHistory() {
		const to = new Date();
		const from = new Date(to.getTime() - SPANS[span]);
		history = await api.get<History>(
			`/monitors/${id}/history?from=${from.toISOString()}&to=${to.toISOString()}`
		);
	}

	async function loadEverything() {
		error = null;
		try {
			await loadCore();
			await loadHistory();

			// Both of these are supplementary: a monitor page that fails entirely
			// because a certificate has never been observed would be useless for
			// the nine monitor types that have none.
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
				// 404 is the ordinary answer for a monitor with no observation.
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
			await goto('/monitors', { replaceState: true });
		} catch (caught) {
			error = caught;
			busy = null;
		}
	}

	$effect(() => {
		// Reading `id` here is what re-runs this when the route parameter changes,
		// so navigating between two monitors reloads rather than showing the first
		// one's data under the second one's name. It is also the *only* thing this
		// effect should depend on: loadEverything reaches loadHistory, which reads
		// the selected range, and tracking that would duplicate the effect below on
		// every range change.
		void id;
		untrack(() => void loadEverything());

		const timer = setInterval(() => void loadCore().catch(() => {}), 15000);
		return () => clearInterval(timer);
	});

	// The range picker, which reloads only the chart.
	$effect(() => {
		void span;
		if (untrack(() => monitor)) void loadHistory().catch(() => {});
	});

	const target = $derived(monitor ? monitorTarget(monitor) : '');
	const canWrite = $derived(session.allows('monitors:write'));
</script>

{#if error && !monitor}
	<ErrorBox {error} onretry={loadEverything} />
{:else if !monitor}
	<Spinner />
{:else}
	<div class="space-y-6">
		<div class="flex flex-wrap items-start justify-between gap-3">
			<div class="min-w-0">
				<a href="/monitors" class="muted text-sm underline">{t('nav.monitors')}</a>
				<h1 class="mt-1 flex flex-wrap items-center gap-3 text-2xl font-semibold">
					{monitor.name}
					<StatusBadge status={monitor.status} />
				</h1>
				<p class="muted mt-1 text-sm break-all">
					{monitor.type}{target ? ` · ${target}` : ''} ·
					{t('monitor.interval', { seconds: formatDuration(monitor.interval_seconds) })}
				</p>
				{#if monitor.description}
					<p class="mt-2 text-sm">{monitor.description}</p>
				{/if}
			</div>

			{#if canWrite}
				<div class="flex flex-wrap gap-2">
					{#if session.can('manual_check')}
						<Button
							loading={busy === 'check'}
							onclick={() => act('check', 'check')}
							title={t('monitors.checkNow')}
						>
							{t('monitors.checkNow')}
						</Button>
					{/if}
					{#if monitor.enabled}
						<Button loading={busy === 'pause'} onclick={() => act('pause', 'pause')}>
							{t('monitors.pause')}
						</Button>
					{:else}
						<Button loading={busy === 'resume'} onclick={() => act('resume', 'resume')}>
							{t('monitors.resume')}
						</Button>
					{/if}
					<Button href="/monitors/{id}/edit">{t('common.edit')}</Button>
					<Button variant="danger" loading={busy === 'delete'} onclick={remove}>
						{t('common.delete')}
					</Button>
				</div>
			{/if}
		</div>

		{#if error}
			<ErrorBox {error} />
		{/if}

		<div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
			{#each ['24h', '7d', '30d'] as window (window)}
				<div class="surface rounded-lg p-4">
					<p class="muted text-sm">{t('monitor.uptime')} · {window}</p>
					<p class="mt-1 text-2xl font-semibold tabular-nums">
						{formatUptime(uptime?.windows?.[window]?.uptime_ratio ?? null)}
					</p>
					{#if uptime?.windows?.[window]}
						<p class="muted text-xs">
							{formatResponseTime(uptime.windows[window].response_time_avg_ms)}
						</p>
					{/if}
				</div>
			{/each}
			<div class="surface rounded-lg p-4">
				<p class="muted text-sm">{t('monitors.lastCheck')}</p>
				<p class="mt-1 text-2xl font-semibold">{formatRelative(monitor.last_check_at)}</p>
				<p class="muted text-xs">
					{t('monitors.nextCheck')}: {formatRelative(monitor.next_check_at)}
				</p>
			</div>
		</div>

		<section class="surface space-y-3 rounded-lg p-4">
			<div class="flex flex-wrap items-center justify-between gap-3">
				<h2 class="font-semibold">{t('monitor.history')}</h2>
				<div class="flex gap-1">
					{#each Object.keys(SPANS) as option (option)}
						<button
							type="button"
							class="rounded-md px-2 py-1 text-xs"
							style={span === option
								? 'background-color: var(--surface-sunken); font-weight: 500'
								: ''}
							aria-pressed={span === option}
							onclick={() => (span = option)}
						>
							{option}
						</button>
					{/each}
				</div>
			</div>
			{#if history}
				<HistoryChart buckets={history.data} />
			{:else}
				<Spinner />
			{/if}
		</section>

		<section class="surface space-y-3 rounded-lg p-4">
			<h2 class="font-semibold">{t('monitor.heartbeats')}</h2>
			{#if beats.length === 0}
				<p class="muted text-sm">{t('monitor.noHeartbeats')}</p>
			{:else}
				<HeartbeatStrip {beats} limit={60} />
				<ul class="divide-y text-sm" style="border-color: var(--border)">
					{#each beats.slice(0, 12) as beat (beat.time)}
						<li class="flex flex-wrap items-baseline gap-x-3 gap-y-1 py-2">
							<StatusBadge status={beat.status} size="sm" />
							<span class="muted tabular-nums" title={formatAbsolute(beat.time)}>
								{formatRelative(beat.time)}
							</span>
							<span class="tabular-nums">{formatResponseTime(beat.response_time_ms)}</span>
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

		{#if certificate}
			<section class="surface space-y-2 rounded-lg p-4">
				<div class="flex flex-wrap items-center justify-between gap-3">
					<h2 class="font-semibold">{t('monitor.certificate')}</h2>
					<!-- The expiry countdown, toned by the same 30-day horizon the
					     overview counts against. -->
					<span
						class="rounded-full px-2.5 py-1 text-sm"
						style="background-color: var(--color-{certificate.days_remaining < 0
							? 'down'
							: certificate.days_remaining <= 14
								? 'down'
								: certificate.days_remaining <= 30
									? 'pending'
									: 'up'}-soft)"
					>
						{t('monitor.certificateExpires', {
							when: formatRelative(certificate.valid_to)
						})}
					</span>
				</div>
				<dl class="grid gap-x-6 gap-y-1 text-sm sm:grid-cols-2">
					{#if certificate.subject}
						<div>
							<dt class="muted inline">{t('monitor.certificateSubject')}:</dt>
							<dd class="inline break-all">{certificate.subject}</dd>
						</div>
					{/if}
					{#if certificate.issuer}
						<div>
							<dt class="muted inline">{t('monitor.certificateIssuer')}:</dt>
							<dd class="inline break-all">{certificate.issuer}</dd>
						</div>
					{/if}
					<div>
						<dt class="muted inline">{t('common.required')}:</dt>
						<dd class="inline">{formatAbsolute(certificate.valid_to)}</dd>
					</div>
					{#if certificate.chain_valid !== undefined}
						<div>
							<dt class="muted inline">{t('monitor.certificateChain')}:</dt>
							<dd class="inline">
								{certificate.chain_valid
									? t('monitor.certificateChainValid')
									: (certificate.chain_error ?? t('common.no'))}
							</dd>
						</div>
					{/if}
					{#if certificate.fingerprint_sha256}
						<div class="sm:col-span-2">
							<dt class="muted inline">{t('monitor.certificateFingerprint')}:</dt>
							<dd class="inline font-mono text-xs break-all">
								{certificate.fingerprint_sha256}
							</dd>
						</div>
					{/if}
				</dl>
				<!-- observed_at means "last confirmed on the wire", to within an hour:
				     the probe resends an unchanged observation hourly rather than on
				     every check (protocol §7.4). Saying so is the difference between a
				     stale-looking timestamp and a wrong one. -->
				<p class="muted text-xs">
					{t('monitor.certificateObserved', { when: formatRelative(certificate.observed_at) })}
				</p>
			</section>
		{/if}
	</div>
{/if}
