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
	 * The public status page.
	 *
	 * A component rather than a route, because it is reachable from two
	 * addresses. `{base_url}/status/{slug}` is the one subscriber mail links to
	 * (docs/api/README.md). The other is the bare root of a page's custom
	 * domain: the server resolves the request's Host, puts the slug in the
	 * application shell, and the root layout renders this — so a customer's
	 * hostname shows the customer's page without an internal slug appearing in
	 * their address bar.
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
	let { slug }: { slug: string } = $props();

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

	const REFRESH_SECONDS = 60;

	/**
	 * Seconds until the next poll, counted down for the visitor.
	 *
	 * Shown because of who is reading this page: somebody waiting for a number to
	 * change, deciding every few seconds whether to hit reload. Telling them the
	 * page refreshes itself, and when, is the difference between a page that
	 * looks frozen and one that is visibly working. It is driven off the same
	 * timer that does the refreshing rather than a second schedule, so it cannot
	 * drift away from the thing it describes.
	 */
	let secondsToRefresh = $state(REFRESH_SECONDS);

	$effect(() => {
		void slug;
		void load();
		secondsToRefresh = REFRESH_SECONDS;
		// A status page is refreshed on its own because the person looking at it is
		// waiting for it to change. Sixty seconds against the server's own
		// thirty-second cache header.
		const timer = setInterval(() => {
			void load();
			secondsToRefresh = REFRESH_SECONDS;
		}, REFRESH_SECONDS * 1000);
		const tick = setInterval(() => {
			secondsToRefresh = Math.max(0, secondsToRefresh - 1);
		}, 1000);
		return () => {
			clearInterval(timer);
			clearInterval(tick);
		};
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

<div class="min-h-full">
	{#if needsPassword}
		<form class="card mx-auto mt-16 max-w-sm space-y-4 p-6" onsubmit={unlock}>
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
		<div class="mx-auto max-w-3xl px-4 py-16 text-center">
			<p class="muted">{t('public.notFound')}</p>
		</div>
	{:else if !statusPage}
		<Spinner />
	{:else}
		{@const sp = statusPage}

		<!-- A full-bleed band with the headline card lifted out of it. The overlap
		     is doing real work: it puts the one sentence a visitor came for above
		     everything else on the page, at a size they can read without reading. -->
		<header
			class="border-b px-4 pt-10 pb-28"
			style="background-color: var(--surface-sunken); border-color: var(--border)"
		>
			<div class="mx-auto flex max-w-3xl flex-wrap items-start justify-between gap-4">
				<div class="min-w-0">
					{#if sp.logo_url}
						<img src={sp.logo_url} alt="" class="mb-3 h-10 object-contain" />
					{/if}
					<h1 class="text-2xl font-bold tracking-tight">{sp.title}</h1>
					{#if sp.description}
						<p class="muted mt-1 text-sm">{sp.description}</p>
					{/if}
				</div>
				<div class="text-right">
					<p class="font-semibold">{t('public.serviceStatus')}</p>
					<p class="muted text-xs">
						{t('public.updated', { when: formatRelative(sp.generated_at) })}
						<span aria-hidden="true">|</span>
						<!-- aria-live is deliberately absent: a screen reader announcing a
						     countdown every second would make the page unusable. -->
						{t('public.nextUpdate', { seconds: secondsToRefresh })}
					</p>
				</div>
			</div>
		</header>

		<div class="mx-auto -mt-20 max-w-3xl px-4 pb-10">
			<div class="card mb-8 flex items-center gap-5 p-6 sm:p-8">
				<!-- Decorative: the sentence beside it says the same thing, so the
				     colour is never the only carrier. -->
				<span
					class="flex h-16 w-16 shrink-0 items-center justify-center rounded-full sm:h-20 sm:w-20"
					style="background-color: var(--color-{headline.tone}-soft)"
					aria-hidden="true"
				>
					<span
						class="h-9 w-9 rounded-full sm:h-11 sm:w-11"
						style="background-color: var(--color-{headline.tone})"
					></span>
				</span>
				<p class="text-2xl font-bold tracking-tight sm:text-3xl">
					{headline.text}
				</p>
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
							<div class="space-y-3 px-5 py-4" style="border-color: var(--border)">
								<div class="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
									<p class="min-w-0 font-medium">
										{monitor.name}
										{#if monitor.uptime_percentage !== null}
											<span class="muted font-normal" aria-hidden="true">|</span>
											<span class="tabular-nums" style="color: var(--color-up)">
												{formatUptime(monitor.uptime_percentage)}
											</span>
										{/if}
									</p>
									<StatusBadge status={monitor.status} size="sm" />
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
		</div>
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
