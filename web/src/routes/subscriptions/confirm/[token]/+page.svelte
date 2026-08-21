<script lang="ts">
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import { t } from '$lib/i18n/index.svelte';
	import Spinner from '$lib/components/Spinner.svelte';
	import Button from '$lib/components/Button.svelte';

	/**
	 * `{base_url}/subscriptions/confirm/{token}` — the second half of double
	 * opt-in (docs/api/README.md).
	 *
	 * This page exists because a link in an inbox is followed with `GET` and the
	 * operation behind it is a `POST`. That is the whole reason it is a frontend
	 * route rather than an API endpoint, and it is why the path is fixed in the
	 * spec rather than chosen here: a link in somebody's mailbox outlives any
	 * amount of refactoring.
	 *
	 * It confirms on load rather than asking for a click. The person has already clicked —
	 * in their mail client — and asking twice is how a confirmation flow loses
	 * people. A link-scanner prefetch reaching here confirms the subscription,
	 * which is acceptable for *this* operation precisely because a scanner
	 * running in the recipient's own mail path is evidence the message arrived at
	 * the address that asked for it. The unsubscribe route makes the opposite
	 * call, and for the opposite reason.
	 */
	const token = $derived(page.params.token!);

	let state = $state<'working' | 'confirmed' | 'failed'>('working');
	let started = false;

	$effect(() => {
		if (started) return;
		started = true;
		(async () => {
			try {
				await api.post(`/public/subscriptions/${encodeURIComponent(token)}`, undefined, {
					expectUnauthorised: true
				});
				state = 'confirmed';
			} catch {
				// Every failure reads the same to the visitor. Distinguishing "this
				// token never existed" from "this token was already used" would turn
				// the page into an oracle for guessing tokens, and neither answer
				// changes what the person should do next.
				state = 'failed';
			}
		})();
	});
</script>

<svelte:head>
	<title>{t('subscription.confirmTitle')}</title>
</svelte:head>

<div class="mx-auto flex min-h-full max-w-md items-center px-4 py-16">
	<div class="card w-full p-8 text-center">
		<h1 class="text-lg font-semibold">{t('subscription.confirmTitle')}</h1>

		{#if state === 'working'}
			<div class="flex justify-center">
				<Spinner label={t('subscription.confirming')} />
			</div>
		{:else if state === 'confirmed'}
			<div
				class="mt-4 rounded-md px-4 py-3 text-sm"
				style="background-color: var(--color-up-soft)"
				role="status"
			>
				{t('subscription.confirmed')}
			</div>
		{:else}
			<div
				class="mt-4 rounded-md px-4 py-3 text-sm"
				style="background-color: var(--color-down-soft)"
				role="alert"
			>
				<p>{t('subscription.confirmFailed')}</p>
				<p class="mt-1">{t('subscription.confirmFailedHint')}</p>
			</div>
		{/if}
	</div>
</div>
