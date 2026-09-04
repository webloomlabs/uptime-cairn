<script lang="ts">
	import { page } from '$app/state';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';

	let { children } = $props();

	/**
	 * The reporting section's own tabs.
	 *
	 * Four screens rather than one, because templates, schedules, runs and brand
	 * profiles are four different jobs done at four different times — writing a
	 * definition, arranging for it to be sent, checking what went out, and
	 * setting up a client's identity. Collapsing them would put a page of run
	 * history in front of somebody who came to change a target.
	 *
	 * Schedules sit second because that is the order the work happens in: a
	 * template is written once and then arranged for, and the arranging is what
	 * turns this section from a report generator into something that sends a
	 * client their report without anybody being at a keyboard.
	 *
	 * Brand profiles carry their own scope, so the tab disappears for a principal
	 * that cannot read them rather than leading to a 403.
	 */
	const tabs = $derived(
		[
			{ href: '/reports', label: t('reports.templates'), exact: true },
			{ href: '/reports/schedules', label: t('reports.schedules'), exact: false },
			{ href: '/reports/runs', label: t('reports.runs'), exact: false },
			{
				href: '/reports/brands',
				label: t('reports.brands'),
				exact: false,
				scope: 'brand_profiles:read'
			}
		].filter((tab) => !tab.scope || session.allows(tab.scope))
	);

	function active(tab: { href: string; exact: boolean }): boolean {
		const here = page.url.pathname;
		return tab.exact ? here === tab.href : here.startsWith(tab.href);
	}
</script>

<nav
	class="mb-5 flex gap-1 border-b"
	style="border-color: var(--border)"
	aria-label={t('reports.title')}
>
	{#each tabs as tab (tab.href)}
		{@const isActive = active(tab)}
		<a
			href={tab.href}
			class="-mb-px border-b-2 px-3 py-2 text-sm transition-colors"
			style={isActive
				? 'border-color: var(--accent); color: var(--text); font-weight: 500'
				: 'border-color: transparent; color: var(--text-muted)'}
			aria-current={isActive ? 'page' : undefined}
		>
			{tab.label}
		</a>
	{/each}
</nav>

{@render children()}
