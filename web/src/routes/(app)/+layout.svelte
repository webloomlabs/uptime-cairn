<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { session } from '$lib/session.svelte';
	import { t } from '$lib/i18n/index.svelte';
	import ThemeToggle from '$lib/components/ThemeToggle.svelte';

	let { children } = $props();

	/**
	 * The authenticated shell, and the gate in front of it.
	 *
	 * The gate is a convenience, not the enforcement: every endpoint behind it
	 * checks its own session and scope on the server. Its job is to stop the
	 * dashboard rendering a page of empty panels and 401s to somebody whose
	 * session has gone, and to send a fresh install to setup rather than to a
	 * sign-in form with no account to sign in to.
	 */
	$effect(() => {
		if (session.setupRequired) {
			goto('/setup', { replaceState: true });
			return;
		}
		if (!session.authenticated) {
			const here = page.url.pathname + page.url.search;
			goto(`/login?next=${encodeURIComponent(here)}`, { replaceState: true });
		}
	});

	type Link = { href: string; label: string; scope?: string; capability?: string };

	// Navigation lists what this build's UI actually has, filtered further by what
	// this principal may read. Two rules, and the second is the one that gets
	// broken: a link to a page that answers 403 reads as a broken product rather
	// than as a scoped key, and a link to a route the frontend does not implement
	// is worse still — it lands on the shell's not-found and looks like data loss.
	//
	// Incidents, status pages and settings have API surface and no screens yet
	// (docs/plans/PHASE-1-TODO.md, "UI"). They get links when they get pages.
	const links = $derived(
		(
			[
				{ href: '/', label: t('nav.dashboard'), scope: 'monitors:read' },
				{ href: '/monitors', label: t('nav.monitors'), scope: 'monitors:read' },
				{
					href: '/notifications',
					label: t('nav.notifications'),
					scope: 'notifications:read',
					capability: 'notifications'
				}
			] satisfies Link[]
		).filter(
			(link) =>
				(!link.scope || session.allows(link.scope)) &&
				(!link.capability || session.can(link.capability))
		)
	);

	function active(href: string): boolean {
		if (href === '/') return page.url.pathname === '/';
		return page.url.pathname === href || page.url.pathname.startsWith(href + '/');
	}
</script>

{#if session.authenticated}
	<a href="#main" class="skip-link">{t('app.skipToContent')}</a>

	<div class="flex min-h-full flex-col">
		<header class="border-b" style="border-color: var(--border)">
			<div class="mx-auto flex max-w-7xl flex-wrap items-center gap-x-6 gap-y-2 px-4 py-3">
				<a href="/" class="flex items-center gap-2 font-semibold">
					<svg width="20" height="20" viewBox="0 0 32 32" aria-hidden="true">
						<path d="M16 5.5 22.5 17h-13L16 5.5Z" fill="var(--accent)" />
						<path d="M16 14.5 21 23H11l5-8.5Z" fill="var(--accent)" opacity="0.6" />
						<rect x="9" y="24.5" width="14" height="2.5" rx="1.25" fill="var(--accent)" />
					</svg>
					{t('app.name')}
				</a>

				<nav aria-label={t('nav.menu')} class="flex flex-1 flex-wrap gap-1">
					{#each links as link (link.href)}
						<a
							href={link.href}
							class="rounded-md px-3 py-1.5 text-sm hover:bg-[var(--surface-sunken)]"
							class:font-medium={active(link.href)}
							style={active(link.href)
								? 'background-color: var(--surface-sunken); color: var(--text)'
								: ''}
							aria-current={active(link.href) ? 'page' : undefined}
						>
							{link.label}
						</a>
					{/each}
				</nav>

				<div class="flex items-center gap-2">
					<ThemeToggle />
					<span class="muted hidden text-sm sm:inline">{session.user?.email}</span>
					<button
						type="button"
						class="rounded-md px-3 py-1.5 text-sm hover:bg-[var(--surface-sunken)]"
						onclick={async () => {
							await session.signOut();
							goto('/login', { replaceState: true });
						}}
					>
						{t('nav.signOut')}
					</button>
				</div>
			</div>
		</header>

		<main id="main" class="mx-auto w-full max-w-7xl flex-1 px-4 py-6">
			{@render children?.()}
		</main>
	</div>
{/if}
