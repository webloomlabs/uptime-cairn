<script lang="ts">
	import '../app.css';
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { session, bindSessionExpiry } from '$lib/session.svelte';
	import { i18n, t } from '$lib/i18n/index.svelte';
	import { theme } from '$lib/theme.svelte';
	import Spinner from '$lib/components/Spinner.svelte';

	let { children } = $props();

	// Referenced so the theme singleton is constructed on first paint rather than
	// on first use, which is what keeps the class in sync after a locale change
	// or a navigation.
	void theme;

	let booted = $state(false);

	$effect(() => {
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
				await session.load();
				booted = true;
			})();
		});
	});
</script>

<svelte:head>
	<title>{t('app.name')}</title>
</svelte:head>

{#if booted}
	{@render children?.()}
{:else}
	<div class="flex min-h-full items-center justify-center">
		<Spinner />
	</div>
{/if}
