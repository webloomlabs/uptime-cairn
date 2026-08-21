<script lang="ts">
	import { untrack } from 'svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import type { Page as ApiPage, StatusPage, Subscriber } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import { formatRelative } from '$lib/format';
	import StatusPageForm from '$lib/components/StatusPageForm.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import Button from '$lib/components/Button.svelte';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';

	const id = $derived(page.params.id!);

	let statusPage = $state<StatusPage | null>(null);
	let error = $state<unknown>(null);

	let subscribers = $state<Subscriber[]>([]);
	let subscribersLoaded = $state(false);
	let subscriberError = $state<unknown>(null);

	let deleting = $state(false);

	const canWrite = $derived(session.allows('status_pages:write'));

	async function load() {
		error = null;
		try {
			statusPage = await api.get<StatusPage>(`/status-pages/${id}`);
		} catch (caught) {
			error = caught;
		}
	}

	async function loadSubscribers() {
		subscriberError = null;
		try {
			subscribers = (
				await api.get<ApiPage<Subscriber>>(`/status-pages/${id}/subscribers`, {
					query: { limit: 100 }
				})
			).data;
		} catch (caught) {
			subscriberError = caught;
		} finally {
			subscribersLoaded = true;
		}
	}

	async function removeSubscriber(subscriber: Subscriber) {
		if (!confirm(t('statusPages.confirmRemoveSubscriber', { target: subscriber.target }))) return;
		try {
			await api.delete(`/status-pages/${id}/subscribers/${subscriber.id}`);
			subscribers = subscribers.filter((held) => held.id !== subscriber.id);
		} catch (caught) {
			subscriberError = caught;
		}
	}

	async function removePage() {
		if (!statusPage) return;
		if (!confirm(t('statusPages.confirmDelete', { title: statusPage.title }))) return;
		deleting = true;
		try {
			await api.delete(`/status-pages/${id}`);
			await goto('/status-pages', { replaceState: true });
		} catch (caught) {
			error = caught;
			deleting = false;
		}
	}

	// `id` is the dependency; the loads are untracked so the state they write
	// cannot feed back into the effect that started them.
	$effect(() => {
		void id;
		untrack(() => {
			void load();
			void loadSubscribers();
		});
	});
</script>

{#if error && !statusPage}
	<ErrorBox {error} onretry={load} />
{:else if !statusPage}
	<Spinner />
{:else}
	<div class="space-y-5">
		<a
			href="/status-pages"
			class="muted inline-flex items-center gap-1 rounded-lg px-2.5 py-1.5 text-sm transition-colors hover:bg-[var(--surface-hover)]"
		>
			<Icon name="chevronLeft" size={16} />
			{t('nav.statusPages')}
		</a>

		<PageTitle title={statusPage.title}>
			{#snippet actions()}
				<Button
					href={statusPage!.custom_domain
						? `https://${statusPage!.custom_domain}`
						: `/status/${statusPage!.slug}`}
					target="_blank"
					rel="noopener noreferrer"
				>
					<Icon name="external" size={16} />
					{t('statusPages.viewPublic')}
				</Button>
			{/snippet}
		</PageTitle>

		{#if error}
			<ErrorBox {error} />
		{/if}

		{#key statusPage.id}
			<StatusPageForm {statusPage} />
		{/key}

		<section class="card space-y-4 p-5">
			<h2 class="font-semibold">{t('statusPages.subscribers')}</h2>

			{#if subscriberError}
				<ErrorBox error={subscriberError} onretry={loadSubscribers} />
			{:else if !subscribersLoaded}
				<Spinner />
			{:else if subscribers.length === 0}
				<p class="muted text-sm">{t('statusPages.noSubscribers')}</p>
			{:else}
				<ul class="divide-y" style="border-color: var(--border)">
					{#each subscribers as subscriber (subscriber.id)}
						<li class="flex flex-wrap items-center gap-x-4 gap-y-1 py-2.5">
							<div class="min-w-0 flex-1">
								<!-- Masked by the server. A page's subscriber list is an export
								     of somebody else's customers, so the dashboard is shown that
								     a row exists and not who it is. -->
								<p class="truncate font-mono text-sm">{subscriber.target}</p>
								<p class="muted text-xs">
									{subscriber.channel}
									{#if subscriber.confirmed}
										· {t('statusPages.confirmedAt', {
											when: formatRelative(subscriber.confirmed_at)
										})}
									{:else}
										· {t('statusPages.unconfirmed')}
									{/if}
								</p>
							</div>
							{#if canWrite}
								<Button size="sm" variant="danger" onclick={() => removeSubscriber(subscriber)}>
									{t('common.delete')}
								</Button>
							{/if}
						</li>
					{/each}
				</ul>
			{/if}
		</section>

		{#if canWrite}
			<section class="card space-y-3 p-5">
				<h2 class="font-semibold">{t('statusPages.dangerZone')}</h2>
				<p class="muted text-sm">{t('statusPages.deleteHint')}</p>
				<Button variant="danger" loading={deleting} onclick={removePage}>
					<Icon name="trash" size={16} />
					{t('statusPages.deletePage')}
				</Button>
			</section>
		{/if}
	</div>
{/if}
