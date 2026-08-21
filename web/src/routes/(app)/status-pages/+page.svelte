<script lang="ts">
	import { untrack } from 'svelte';
	import { api } from '$lib/api';
	import type { Page as ApiPage } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';

	type StatusPage = {
		id: string;
		slug: string;
		title: string;
		description: string | null;
		published: boolean;
		visibility: string;
		custom_domain: string | null;
		subscriptions_enabled: boolean;
		sections: { name: string; monitor_ids: string[] }[];
	};

	let pages = $state<StatusPage[]>([]);
	let loading = $state(true);
	let error = $state<unknown>(null);

	async function load() {
		loading = true;
		error = null;
		try {
			pages = (await api.get<ApiPage<StatusPage>>('/status-pages?limit=100')).data;
		} catch (caught) {
			error = caught;
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		untrack(() => void load());
	});

	function monitorCount(page: StatusPage): number {
		return page.sections.reduce((total, section) => total + section.monitor_ids.length, 0);
	}

	/** A page with a custom domain is served from it; otherwise from this install. */
	function publicHref(page: StatusPage): string {
		return page.custom_domain ? `https://${page.custom_domain}` : `/status/${page.slug}`;
	}
</script>

<PageTitle title={t('nav.statusPages')} />

{#if loading}
	<Spinner />
{:else if error}
	<ErrorBox {error} onretry={load} />
{:else if pages.length === 0}
	<div class="card px-4 py-14 text-center">
		<p class="font-medium">{t('statusPages.empty')}</p>
		<p class="muted mx-auto mt-1 max-w-md text-sm">{t('statusPages.emptyHint')}</p>
	</div>
{:else}
	<div class="card overflow-x-auto">
		<table class="w-full text-sm">
			<thead>
				<tr class="border-b text-left" style="border-color: var(--border)">
					{#each [t('statusPages.name'), t('statusPages.access'), t('statusPages.state'), t('statusPages.monitors'), ''] as heading, index (index)}
						<th class="muted px-4 py-3 text-xs font-medium whitespace-nowrap">{heading}</th>
					{/each}
				</tr>
			</thead>
			<tbody>
				{#each pages as statusPage (statusPage.id)}
					<tr class="border-b last:border-0" style="border-color: var(--border)">
						<td class="px-4 py-3.5">
							<span class="flex items-center gap-3">
								<span style="color: var(--color-up)"><Icon name="status" size={18} /></span>
								<span class="min-w-0">
									<span class="block truncate font-medium">{statusPage.title}</span>
									<span class="muted block truncate text-xs">
										{statusPage.custom_domain ?? `/status/${statusPage.slug}`}
										{#if monitorCount(statusPage) === 0}
											· {t('statusPages.noMonitors')}
										{/if}
									</span>
								</span>
							</span>
						</td>
						<td class="muted px-4 py-3.5 whitespace-nowrap">
							<span class="inline-flex items-center gap-1.5">
								{#if statusPage.visibility !== 'public'}
									<Icon name="shield" size={14} />
								{/if}
								{t(`statusPages.visibility.${statusPage.visibility}`)}
							</span>
						</td>
						<td class="px-4 py-3.5 whitespace-nowrap">
							<span
								style={statusPage.published ? 'color: var(--color-up)' : 'color: var(--text-muted)'}
							>
								{statusPage.published ? t('statusPages.published') : t('statusPages.draft')}
							</span>
						</td>
						<td class="px-4 py-3.5 tabular-nums whitespace-nowrap">
							{monitorCount(statusPage)}
						</td>
						<td class="px-4 py-3.5 text-right whitespace-nowrap">
							<a
								href={publicHref(statusPage)}
								target="_blank"
								rel="noopener noreferrer"
								class="muted inline-flex rounded-lg p-2 transition-colors hover:bg-[var(--surface-hover)]"
								aria-label={t('statusPages.open', { title: statusPage.title })}
							>
								<Icon name="external" size={16} />
							</a>
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>

	{#if session.info && !session.can('subscriber_delivery')}
		<!-- Reported rather than assumed: a subscribe box the install cannot honour
		     is worse than none, because the person who used it believes they will be
		     told. /system/info answers this honestly and the page repeats it. -->
		<p class="muted mt-4 text-xs">{t('statusPages.deliveryOff')}</p>
	{/if}
{/if}
