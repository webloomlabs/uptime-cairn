<script lang="ts">
	import '../app.css';
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { session, bindSessionExpiry } from '$lib/session.svelte';
	import { i18n, t } from '$lib/i18n/index.svelte';
	import { theme } from '$lib/theme.svelte';
	import Spinner from '$lib/components/Spinner.svelte';
	import StatusPageView from '$lib/components/StatusPageView.svelte';

	let { children } = $props();

	/**
	 * A custom-domain status page, resolved by the server.
	 *
	 * The server matches the request's Host against the status pages' custom
	 * domains and, on a match, serves the application shell with the slug in it.
	 * This reads that and renders the page — at whatever path was asked for, so
	 * the customer's hostname shows the customer's page at its bare root rather
	 * than redirecting to `/status/{slug}` with an internal slug in the address
	 * bar. See internal/api/domains.go for why it cannot be a proxy rewrite.
	 *
	 * Read once, not reactively: it is a fact about which document the server
	 * sent, and it cannot change without a full page load.
	 */
	const customDomainSlug =
		typeof window === 'undefined'
			? null
			: ((window as unknown as { __cairnStatusPage?: string }).__cairnStatusPage ?? null);

	// Referenced so the theme singleton is constructed on first paint rather than
	// on first use, which is what keeps the class in sync after a locale change
	// or a navigation.
	void theme;

	let booted = $state(false);

	$effect(() => {
		if (customDomainSlug) return;
		// A session that expires while somebody is looking at a dashboard should
		// return them to the sign-in page with a reason, not throw into a
		// component. `next` brings them back to where they were.
		bindSessionExpiry(() => {
			const here = page.url.pathname + page.url.search;
			goto(`/login?expired=1&next=${encodeURIComponent(here)}`, { replaceState: true });
		});
	});

	$effect(() => {
		// Guarded and untracked: `booted` is both the guard and something this
		// effect writes, and an effect that invalidates itself is aborted by Svelte
		// rather than merely re-run.
		untrack(() => {
			if (booted) return;
			(async () => {
				await i18n.detect();
				// The session is deliberately not loaded on a custom domain. A
				// status page is unauthenticated by construction, its visitor is
				// a customer rather than an operator, and asking whether they are
				// signed in would be a 401 in the console on every load of a page
				// that is working perfectly.
				if (!customDomainSlug) await session.load();
				booted = true;
			})();
		});
	});
</script>

<svelte:head>
	<title>{t('app.name')}</title>
</svelte:head>

{#if !booted}
	<div class="flex min-h-full items-center justify-center">
		<Spinner />
	</div>
{:else if customDomainSlug}
	<StatusPageView slug={customDomainSlug} />
{:else}
	{@render children?.()}
{/if}
