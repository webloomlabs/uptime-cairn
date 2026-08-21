<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { session } from '$lib/session.svelte';
	import { t } from '$lib/i18n/index.svelte';
	import ThemeToggle from '$lib/components/ThemeToggle.svelte';
	import Logo from '$lib/components/Logo.svelte';
	import Icon from '$lib/components/Icon.svelte';

	let { children } = $props();

	/**
	 * The authenticated shell: a fixed left rail, and the page beside it.
	 *
	 * The gate in front of it is a convenience, not the enforcement — every
	 * endpoint checks its own session and scope on the server. Its job is to stop
	 * the dashboard rendering a page of empty panels and 401s to somebody whose
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

	type Link = { href: string; label: string; icon: string; scope?: string; capability?: string };

	// Filtered by what this principal may read and what this build actually has.
	// A link to a page that answers 403 reads as a broken product rather than as a
	// scoped key, and a link to a route the frontend does not implement is worse
	// still — it lands on the shell's not-found and looks like data loss.
	const links = $derived(
		(
			[
				{ href: '/', label: t('nav.monitoring'), icon: 'monitoring', scope: 'monitors:read' },
				{
					href: '/incidents',
					label: t('nav.incidents'),
					icon: 'incidents',
					scope: 'incidents:read',
					capability: 'incidents'
				},
				{
					href: '/status-pages',
					label: t('nav.statusPages'),
					icon: 'status',
					scope: 'status_pages:read',
					capability: 'status_pages'
				},
				{
					href: '/notifications',
					label: t('nav.notifications'),
					icon: 'notifications',
					scope: 'notifications:read',
					capability: 'notifications'
				},
				// Groups and tags are one screen because they are one job. The link
				// takes a single scope, so it is gated on groups:read; the page
				// itself checks both and renders only the half the principal may
				// read. A cookie session holds its whole role's scope set, so the
				// two come together for everyone who actually browses this nav.
				{
					href: '/groups',
					label: t('nav.taxonomy'),
					icon: 'tag',
					scope: 'groups:read'
				},
				// Import sits under settings:write rather than imports:write on
				// purpose: it is a one-off migration screen, and a key scoped to
				// imports alone is a machine credential rather than somebody
				// browsing. The endpoint still checks imports:write.
				{
					href: '/import',
					label: t('nav.import'),
					icon: 'refresh',
					scope: 'imports:write'
				},
				{
					href: '/settings',
					label: t('nav.settings'),
					icon: 'settings',
					scope: 'settings:read'
				}
			] satisfies Link[]
		).filter(
			(link) =>
				(!link.scope || session.allows(link.scope)) &&
				(!link.capability || session.can(link.capability))
		)
	);

	function active(href: string): boolean {
		if (href === '/') return page.url.pathname === '/' || page.url.pathname.startsWith('/monitors');
		return page.url.pathname === href || page.url.pathname.startsWith(href + '/');
	}

	const initials = $derived(
		(session.user?.name ?? session.user?.email ?? '?')
			.split(/[\s@.]+/)
			.filter(Boolean)
			.slice(0, 2)
			.map((part) => part[0]?.toUpperCase() ?? '')
			.join('')
	);

	let open = $state(false);
</script>

{#if session.authenticated}
	<a href="#main" class="skip-link">{t('app.skipToContent')}</a>

	<div class="flex min-h-full">
		<!-- The rail. Fixed on desktop, a drawer under it. -->
		<aside
			class="fixed inset-y-0 left-0 z-30 flex w-64 flex-col border-r transition-transform lg:translate-x-0"
			class:-translate-x-full={!open}
			style="border-color: var(--border); background-color: var(--surface-sunken)"
		>
			<div class="px-5 pt-6 pb-8">
				<a
					href="/"
					class="flex items-center gap-2.5 text-lg font-semibold tracking-tight"
					style="color: var(--color-up)"
				>
					<Logo size={16} />
					<span style="color: var(--text)">{t('app.name')}</span>
				</a>
			</div>

			<nav aria-label={t('nav.menu')} class="flex-1 space-y-1 px-3">
				{#each links as link (link.href)}
					{@const isActive = active(link.href)}
					<a
						href={link.href}
						onclick={() => (open = false)}
						class="flex items-center gap-3 rounded-lg px-3 py-2.5 text-sm transition-colors"
						style={isActive
							? 'background-color: var(--surface-hover); color: var(--text); font-weight: 500'
							: 'color: var(--text-muted)'}
						aria-current={isActive ? 'page' : undefined}
					>
						<span style={isActive ? 'color: var(--color-up)' : ''}>
							<Icon name={link.icon} size={19} />
						</span>
						{link.label}
					</a>
				{/each}
			</nav>

			<div class="space-y-3 px-3 pt-4 pb-5">
				<div class="flex items-center gap-3 px-2">
					<span
						class="flex h-9 w-9 shrink-0 items-center justify-center rounded-full text-xs font-semibold"
						style="background-color: var(--surface-hover); color: var(--text)"
						aria-hidden="true"
					>
						{initials}
					</span>
					<span class="min-w-0 flex-1">
						<span class="block truncate text-sm font-medium">
							{session.user?.name || session.user?.email}
						</span>
						{#if session.user?.name}
							<span class="muted block truncate text-xs">{session.user.email}</span>
						{/if}
					</span>
				</div>

				<div class="flex items-center gap-1 px-1">
					<ThemeToggle />
					<button
						type="button"
						class="flex flex-1 items-center gap-2 rounded-lg px-3 py-2 text-sm transition-colors hover:bg-[var(--surface-hover)]"
						style="color: var(--text-muted)"
						onclick={async () => {
							await session.signOut();
							goto('/login', { replaceState: true });
						}}
					>
						<Icon name="logout" size={17} />
						{t('nav.signOut')}
					</button>
				</div>
			</div>
		</aside>

		{#if open}
			<button
				type="button"
				class="fixed inset-0 z-20 bg-black/50 lg:hidden"
				aria-label={t('common.close')}
				onclick={() => (open = false)}
			></button>
		{/if}

		<div class="flex min-w-0 flex-1 flex-col lg:pl-64">
			<button
				type="button"
				class="m-3 w-fit rounded-lg p-2 lg:hidden"
				style="border: 1px solid var(--border)"
				aria-label={t('nav.menu')}
				aria-expanded={open}
				onclick={() => (open = true)}
			>
				<Icon name="filter" size={20} />
			</button>

			<main id="main" class="min-w-0 flex-1 px-4 py-6 sm:px-8 sm:py-8">
				{@render children?.()}
			</main>
		</div>
	</div>
{/if}
