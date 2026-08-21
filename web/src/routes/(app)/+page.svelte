<script lang="ts">
	import { untrack } from 'svelte';
	import { api } from '$lib/api';
	import type { Overview } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import { MonitorList } from '$lib/monitorlist.svelte';
	import { formatRelative } from '$lib/format';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Button from '$lib/components/Button.svelte';
	import MonitorRows from '$lib/components/MonitorRows.svelte';

	let overview = $state<Overview | null>(null);
	let error = $state<unknown>(null);

	/**
	 * The dashboard shows what is wrong, not everything.
	 *
	 * The headline counts come from /overview, which is computed server-side —
	 * summing whatever the client happens to have loaded would couple a global
	 * number to viewport state, which ADR-004 rules out explicitly. Underneath
	 * them is a `status=down` view, because the list of things currently broken is
	 * the only list worth opening a monitoring tool to see.
	 */
	const down = new MonitorList(25, { status: ['down'] });

	async function loadOverview() {
		try {
			overview = await api.get<Overview>('/overview');
			error = null;
		} catch (caught) {
			error = caught;
		}
	}

	// untrack, because this is an imperative bootstrap rather than a computation:
	// the calls inside read and write the very state they manage, and tracking any
	// of it turns mount into a loop. What should re-run this effect is the route
	// changing, and this route has no parameters — so it should run exactly once.
	$effect(() => {
		untrack(() => {
			void loadOverview();
			void down.load();
			down.start();
		});

		const timer = setInterval(loadOverview, 15000);
		return () => {
			down.stop();
			clearInterval(timer);
		};
	});

	const cards = $derived(
		overview
			? [
					{ label: t('overview.total'), value: overview.monitors.total, tone: null },
					{ label: t('status.up'), value: overview.monitors.up, tone: 'up' },
					{ label: t('status.down'), value: overview.monitors.down, tone: 'down' },
					{ label: t('status.pending'), value: overview.monitors.pending, tone: 'pending' },
					{
						label: t('status.maintenance'),
						value: overview.monitors.maintenance,
						tone: 'maintenance'
					},
					{ label: t('status.paused'), value: overview.monitors.paused, tone: 'paused' }
				]
			: []
	);
</script>

<div class="space-y-8">
	<div class="flex flex-wrap items-center justify-between gap-3">
		<h1 class="text-2xl font-semibold">{t('overview.title')}</h1>
		{#if session.allows('monitors:write')}
			<Button href="/monitors/new" variant="primary">{t('monitors.new')}</Button>
		{/if}
	</div>

	{#if error}
		<ErrorBox {error} onretry={loadOverview} />
	{:else if !overview}
		<Spinner />
	{:else}
		<div class="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
			{#each cards as card (card.label)}
				<div class="surface rounded-lg p-4">
					<p class="muted flex items-center gap-1.5 text-sm">
						{#if card.tone}
							<span
								class="inline-block h-2 w-2 rounded-full"
								style="background-color: var(--color-{card.tone})"
								aria-hidden="true"
							></span>
						{/if}
						{card.label}
					</p>
					<p class="mt-1 text-2xl font-semibold tabular-nums">{card.value}</p>
				</div>
			{/each}
		</div>

		<div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
			{#each [{ label: t('overview.activeIncidents'), value: overview.active_incidents, hint: null }, { label: t('overview.maintenance'), value: overview.active_maintenance_windows, hint: null }, { label: t('overview.certificatesExpiring'), value: overview.certificates_expiring_soon, hint: t('overview.expiringWindow') }, { label: t('overview.domainsExpiring'), value: overview.domains_expiring_soon, hint: t('overview.expiringWindow') }] as card (card.label)}
				<div class="surface rounded-lg p-4">
					<p class="muted text-sm">{card.label}</p>
					<p class="mt-1 text-2xl font-semibold tabular-nums">{card.value}</p>
					{#if card.hint}
						<p class="muted text-xs">{card.hint}</p>
					{/if}
				</div>
			{/each}
		</div>

		<p class="muted text-xs">
			{t('public.updated', { when: formatRelative(overview.generated_at) })}
		</p>
	{/if}

	<section class="space-y-3">
		<div class="flex items-center justify-between gap-3">
			<h2 class="text-lg font-semibold">{t('status.down')}</h2>
			<a href="/monitors" class="text-sm underline">{t('nav.monitors')}</a>
		</div>

		{#if down.loading}
			<Spinner />
		{:else if down.error}
			<ErrorBox error={down.error} onretry={() => down.load()} />
		{:else if down.monitors.length === 0}
			<p class="muted surface rounded-lg px-4 py-6 text-sm">{t('public.allOperational')}</p>
		{:else}
			<MonitorRows monitors={down.monitors} />
		{/if}
	</section>
</div>
