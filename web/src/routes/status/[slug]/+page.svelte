<script lang="ts">
	import { page } from '$app/state';
	import { api, ApiError } from '$lib/api';
	import type { PublicIncident, PublicStatusPage } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { formatAbsolute, formatRelative, formatResponseTime, formatUptime } from '$lib/format';
	import Spinner from '$lib/components/Spinner.svelte';
	import Button from '$lib/components/Button.svelte';
	import Field from '$lib/components/Field.svelte';
	import StatusBadge from '$lib/components/StatusBadge.svelte';
	import UptimeBar from '$lib/components/UptimeBar.svelte';

	/**
	 * The public status page — `{base_url}/status/{slug}`, the address subscriber
	 * mail links to (docs/api/README.md).
	 *
	 * Unauthenticated by construction: a status page whose audience needs a
	 * credential is not a status page. It reads the API's own projection, which is
	 * built from a separate shape rather than filtered from a monitor read, so
	 * there is nothing here that could leak a target, a credential, or a
	 * configuration even by accident.
	 *
	 * The visitor here is having a bad day and is not an operator. So: the
	 * headline is a sentence, not a count; a monitor is named and nothing else;
	 * and a day with no data is drawn as absent rather than as an outage.
	 */
	const slug = $derived(page.params.slug!);

	let statusPage = $state<PublicStatusPage | null>(null);
	let error = $state<unknown>(null);
	let needsPassword = $state(false);
	let password = $state('');
	let unlocking = $state(false);
	let unlockError = $state<string | null>(null);

	let subscribeTarget = $state('');
	let subscribing = $state(false);
	let subscribed = $state(false);
	let subscribeError = $state<string | null>(null);

	async function load() {
		error = null;
		try {
			statusPage = await api.get<PublicStatusPage>(`/public/status-pages/${slug}`, {
				expectUnauthorised: true
			});
			needsPassword = false;
		} catch (caught) {
			if (caught instanceof ApiError && caught.is('password-required')) {
				needsPassword = true;
				return;
			}
			error = caught;
		}
	}

	async function unlock(event: SubmitEvent) {
		event.preventDefault();
		unlocking = true;
		unlockError = null;
		try {
			await api.post(
				`/public/status-pages/${slug}/authenticate`,
				{ password },
				{
					expectUnauthorised: true
				}
			);
			password = '';
			await load();
		} catch (caught) {
			unlockError =
				caught instanceof ApiError
					? caught.problem.detail || caught.problem.title
					: t('error.unexpected');
		} finally {
			unlocking = false;
		}
	}

	async function subscribe(event: SubmitEvent) {
		event.preventDefault();
		subscribing = true;
		subscribeError = null;
		try {
			await api.post(`/public/status-pages/${slug}/subscribers`, {
				channel: 'email',
				target: subscribeTarget
			});
			// Deliberately the same message whether this was a new subscription or a
			// repeat: the server does not resend on a conflict, and telling a
			// stranger which addresses are already subscribed to somebody else's
			// page would make the form an address oracle.
			subscribed = true;
			subscribeTarget = '';
		} catch (caught) {
			subscribeError =
				caught instanceof ApiError
					? caught.problem.detail || t('public.subscribeFailed')
					: t('public.subscribeFailed');
		} finally {
			subscribing = false;
		}
	}

	$effect(() => {
		void slug;
		void load();
		// A status page is refreshed on its own because the person looking at it is
		// waiting for it to change. Sixty seconds against the server's own
		// thirty-second cache header.
		const timer = setInterval(() => void load(), 60000);
		return () => clearInterval(timer);
	});

	// The five values `overall_status` actually takes (internal/model/statuspage.go).
	// Partial and major are kept apart deliberately: they are computed by a rule —
	// major once at least half of what the page covers is down — and collapsing
	// them into "some systems are down" throws away the only thing on this page
	// that tells a visitor how bad it is.
	const headline = $derived.by(() => {
		switch (statusPage?.overall_status) {
			case 'operational':
				return { text: t('public.allOperational'), tone: 'up' };
			case 'degraded':
				return { text: t('public.degraded'), tone: 'pending' };
			case 'partial_outage':
				return { text: t('public.partialOutage'), tone: 'pending' };
			case 'major_outage':
				return { text: t('public.majorOutage'), tone: 'down' };
			case 'maintenance':
				return { text: t('public.underMaintenance'), tone: 'maintenance' };
			default:
				return { text: t('status.unknown'), tone: 'unknown' };
		}
	});

	function impactTone(incident: PublicIncident): string {
		if (incident.resolved_at) return 'up';
		return incident.impact === 'critical' || incident.impact === 'major' ? 'down' : 'pending';
	}
</script>

<svelte:head>
	<title>{statusPage?.title ?? t('app.name')}</title>
	{#if statusPage?.favicon_url}
		<link rel="icon" href={statusPage.favicon_url} />
	{/if}
</svelte:head>

<div class="mx-auto max-w-3xl px-4 py-10">
	{#if needsPassword}
		<form class="card mx-auto max-w-sm space-y-4 p-6" onsubmit={unlock}>
			<h1 class="font-semibold">{t('auth.password')}</h1>
			<Field label={t('auth.password')} id="page-password">
				{#snippet children({ id })}
					<input {id} type="password" class="field" bind:value={password} required />
				{/snippet}
			</Field>
			{#if unlockError}
				<p class="text-sm" style="color: var(--color-down)" role="alert">{unlockError}</p>
			{/if}
			<Button type="submit" variant="primary" loading={unlocking} class="w-full">
				{t('common.close')}
			</Button>
		</form>
	{:else if error}
		<div class="py-16 text-center">
			<p class="muted">{t('public.notFound')}</p>
		</div>
	{:else if !statusPage}
		<Spinner />
	{:else}
		{@const sp = statusPage}
		<header class="mb-8 text-center">
			{#if sp.logo_url}
				<img src={sp.logo_url} alt="" class="mx-auto mb-4 h-12 object-contain" />
			{/if}
			<h1 class="text-2xl font-semibold">{sp.title}</h1>
			{#if sp.description}
				<p class="muted mt-1">{sp.description}</p>
			{/if}
		</header>

		<div
			class="mb-8 rounded-lg px-5 py-4 text-center text-lg font-medium"
			style="background-color: var(--color-{headline.tone}-soft)"
		>
			{headline.text}
		</div>

		{#if sp.active_incidents.length}
			<section class="mb-8 space-y-3">
				<h2 class="font-semibold">{t('public.activeIncidents')}</h2>
				{#each sp.active_incidents as incident (incident.id)}
					{@render incidentCard(incident)}
				{/each}
			</section>
		{/if}

		{#if sp.scheduled_maintenance.length}
			<section class="mb-8 space-y-3">
				<h2 class="font-semibold">{t('public.scheduledMaintenance')}</h2>
				{#each sp.scheduled_maintenance as window (window.title + window.starts_at)}
					<div class="card p-5">
						<p class="font-medium">{window.title}</p>
						<p class="muted text-sm">
							{formatAbsolute(window.starts_at)}
							{#if window.ends_at}— {formatAbsolute(window.ends_at)}{/if}
						</p>
						{#if window.description}
							<p class="mt-2 text-sm">{window.description}</p>
						{/if}
					</div>
				{/each}
			</section>
		{/if}

		{#each sp.sections as section (section.name)}
			<section class="mb-6">
				<h2 class="mb-2 font-semibold">{section.name}</h2>
				{#if section.description}
					<p class="muted mb-2 text-sm">{section.description}</p>
				{/if}
				<div class="card divide-y" style="border-color: var(--border)">
					{#each section.monitors as monitor (monitor.id)}
						<div class="space-y-2 p-4" style="border-color: var(--border)">
							<div class="flex flex-wrap items-baseline justify-between gap-2">
								<span class="font-medium">{monitor.name}</span>
								<span class="flex items-center gap-3">
									{#if monitor.uptime_percentage !== null}
										<span class="muted text-sm tabular-nums">
											{formatUptime(monitor.uptime_percentage)}
										</span>
									{/if}
									<StatusBadge status={monitor.status} size="sm" />
								</span>
							</div>
							{#if monitor.description}
								<p class="muted text-sm">{monitor.description}</p>
							{/if}
							{#if monitor.uptime_bar?.length}
								<UptimeBar entries={monitor.uptime_bar} />
							{/if}
						</div>
					{/each}
				</div>
			</section>
		{/each}

		<section class="mb-8 space-y-3">
			<h2 class="font-semibold">{t('public.pastIncidents')}</h2>
			{#if sp.recent_incidents.length === 0}
				<p class="muted card p-5 text-sm">{t('public.noIncidents')}</p>
			{:else}
				{#each sp.recent_incidents as incident (incident.id)}
					{@render incidentCard(incident)}
				{/each}
			{/if}
		</section>

		{#if sp.subscriptions_enabled}
			<section class="card mb-8 p-6">
				<h2 class="font-semibold">{t('public.subscribe')}</h2>
				{#if subscribed}
					<p class="mt-2 text-sm">{t('public.subscribeSent')}</p>
				{:else}
					<form class="mt-3 flex flex-wrap gap-2" onsubmit={subscribe}>
						<label class="flex-1" style="min-width: 12rem">
							<span class="sr-only">{t('public.subscribeEmail')}</span>
							<input
								type="email"
								class="field"
								bind:value={subscribeTarget}
								placeholder={t('public.subscribeEmail')}
								required
							/>
						</label>
						<Button type="submit" variant="primary" loading={subscribing}>
							{t('public.subscribeSubmit')}
						</Button>
					</form>
					{#if subscribeError}
						<p class="mt-2 text-sm" style="color: var(--color-down)" role="alert">
							{subscribeError}
						</p>
					{/if}
				{/if}
			</section>
		{/if}

		<footer class="muted space-y-1 text-center text-xs">
			{#if sp.footer_text}
				<p>{sp.footer_text}</p>
			{/if}
			<p>{t('public.updated', { when: formatRelative(sp.generated_at) })}</p>
			{#if sp.show_powered_by}
				<p>{t('public.poweredBy')}</p>
			{/if}
		</footer>
	{/if}
</div>

{#snippet incidentCard(incident: PublicIncident)}
	<article class="card p-5">
		<div class="flex flex-wrap items-baseline justify-between gap-2">
			<h3 class="font-medium">{incident.title}</h3>
			<span
				class="rounded-full px-2 py-0.5 text-xs"
				style="background-color: var(--color-{impactTone(incident)}-soft)"
			>
				{incident.state} · {incident.impact}
			</span>
		</div>
		<p class="muted mt-1 text-xs">
			{formatAbsolute(incident.started_at)}
			{#if incident.resolved_at}— {formatAbsolute(incident.resolved_at)}{/if}
		</p>
		{#if incident.updates.length}
			<ol class="mt-3 space-y-2 border-l pl-3 text-sm" style="border-color: var(--border)">
				{#each incident.updates as update (update.created_at)}
					<li>
						<p class="muted text-xs">
							{#if update.state}<span class="font-medium">{update.state}</span> ·
							{/if}
							{formatAbsolute(update.created_at)}
						</p>
						<p>{update.body}</p>
					</li>
				{/each}
			</ol>
		{/if}
	</article>
{/snippet}
