<script lang="ts">
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import { t } from '$lib/i18n/index.svelte';
	import Button from '$lib/components/Button.svelte';

	/**
	 * `{base_url}/subscriptions/unsubscribe/{token}` — the link at the foot of
	 * every message (docs/api/README.md).
	 *
	 * Unlike the confirmation route, this one asks before it acts, and the reason
	 * is the same one that kept `List-Unsubscribe` off RFC 8058 one-click: a
	 * `GET` to this address is made by things that are not the recipient. Mail
	 * clients prefetch, security appliances follow every link in a message to see
	 * where it goes, and archivers crawl. Unsubscribing on load would quietly
	 * remove people who never clicked anything, and they would not find out until
	 * they missed an outage.
	 *
	 * So the destructive call happens on a real button press. One press — not a
	 * confirmation dialogue on top of it, because somebody who arrived here
	 * wanting out should get out.
	 */
	const token = $derived(page.params.token!);

	let state = $state<'ready' | 'working' | 'done' | 'failed'>('ready');

	async function unsubscribe() {
		state = 'working';
		try {
			await api.delete(`/public/subscriptions/${encodeURIComponent(token)}`, {
				expectUnauthorised: true
			});
			state = 'done';
		} catch {
			// A spent token and a token that never existed read the same, for the
			// same reason as the confirmation route — and here the outcome the
			// visitor cares about is identical either way: no more messages.
			state = 'failed';
		}
	}
</script>

<svelte:head>
	<title>{t('subscription.unsubscribeTitle')}</title>
</svelte:head>

<div class="mx-auto flex min-h-full max-w-md items-center px-4 py-16">
	<div class="card w-full p-8 text-center">
		<h1 class="text-lg font-semibold">{t('subscription.unsubscribeTitle')}</h1>

		{#if state === 'done'}
			<div
				class="mt-4 rounded-md px-4 py-3 text-sm"
				style="background-color: var(--color-up-soft)"
				role="status"
			>
				{t('subscription.unsubscribed')}
			</div>
		{:else if state === 'failed'}
			<div
				class="mt-4 rounded-md px-4 py-3 text-sm"
				style="background-color: var(--color-pending-soft)"
				role="alert"
			>
				<p>{t('subscription.unsubscribeFailed')}</p>
				<p class="mt-1">{t('subscription.unsubscribeFailedHint')}</p>
			</div>
		{:else}
			<p class="muted mt-2 text-sm">{t('subscription.unsubscribePrompt')}</p>
			<div class="mt-5">
				<Button
					variant="danger"
					loading={state === 'working'}
					onclick={unsubscribe}
					class="w-full justify-center"
				>
					{t('subscription.unsubscribeConfirm')}
				</Button>
			</div>
		{/if}
	</div>
</div>
