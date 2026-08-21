<script lang="ts">
	import { untrack } from 'svelte';
	import { api } from '$lib/api';
	import type { Page as ApiPage, StatusPage } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import Button from '$lib/components/Button.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';

	let pages = $state<StatusPage[]>([]);
	let loading = $state(true);
	let error = $state<unknown>(null);

	const canWrite = $derived(session.allows('status_pages:write'));

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

	async function remove(statusPage: StatusPage) {
		if (!confirm(t('statusPages.confirmDelete', { title: statusPage.title }))) return;
		try {
			await api.delete(`/status-pages/${statusPage.id}`);
			pages = pages.filter((held) => held.id !== statusPage.id);
		} catch (caught) {
			error = caught;
		}
	}

	$effect(() => {
		untrack(() => void load());
	});

	function monitorCount(statusPage: StatusPage): number {
		return statusPage.sections.reduce((total, section) => total + section.monitor_ids.length, 0);
	}

	/** A page with a custom domain is served from it; otherwise from this install. */
	function publicHref(statusPage: StatusPage): string {
		return statusPage.custom_domain
			? `https://${statusPage.custom_domain}`
			: `/status/${statusPage.slug}`;
	}
</script>

<PageTitle title={t('nav.statusPages')}>
	{#snippet actions()}
		{#if canWrite}
			<Button variant="primary" href="/status-pages/new">
				<Icon name="plus" size={16} />
				{t('statusPages.new')}
			</Button>
		{/if}
	{/snippet}
</PageTitle>

{#if loading}
	<Spinner />
{:else if error}
	<ErrorBox {error} onretry={load} />
{:else if pages.length === 0}
	<div class="card px-4 py-14 text-center">
		<p class="font-medium">{t('statusPages.empty')}</p>
		<p class="muted mx-auto mt-1 max-w-md text-sm">{t('statusPages.emptyHint')}</p>
		{#if canWrite}
			<div class="mt-4">
				<Button variant="primary" href="/status-pages/new">
					<Icon name="plus" size={16} />
					{t('statusPages.new')}
				</Button>
			</div>
		{/if}
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
							<a
								class="flex items-center gap-3 hover:underline"
								href="/status-pages/{statusPage.id}/edit"
							>
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
							</a>
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
							<span class="inline-flex items-center gap-1">
								<a
									href={publicHref(statusPage)}
									target="_blank"
									rel="noopener noreferrer"
									class="muted inline-flex rounded-lg p-2 transition-colors hover:bg-[var(--surface-hover)]"
									aria-label={t('statusPages.open', { title: statusPage.title })}
								>
									<Icon name="external" size={16} />
								</a>
								{#if canWrite}
									<a
										href="/status-pages/{statusPage.id}/edit"
										class="muted inline-flex rounded-lg p-2 transition-colors hover:bg-[var(--surface-hover)]"
										aria-label={t('statusPages.editPage', { title: statusPage.title })}
									>
										<Icon name="edit" size={16} />
									</a>
									<button
										type="button"
										class="inline-flex rounded-lg p-2 transition-colors hover:bg-[var(--color-down-soft)]"
										style="color: var(--color-down)"
										onclick={() => remove(statusPage)}
										aria-label={t('statusPages.deleteNamed', { title: statusPage.title })}
									>
										<Icon name="trash" size={16} />
									</button>
								{/if}
							</span>
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
