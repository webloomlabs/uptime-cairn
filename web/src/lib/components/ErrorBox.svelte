<script lang="ts">
	import { ApiError } from '$lib/api';
	import { t } from '$lib/i18n/index.svelte';
	import Button from './Button.svelte';

	/**
	 * A failed request, rendered for someone who has to decide what to do next.
	 *
	 * The problem document's `detail` is shown when there is one, because the
	 * server writes it for exactly this moment and it is more specific than
	 * anything the client could say. Validation problems are handled by the form
	 * that raised them, not here — a list of pointers is not a message.
	 */
	let { error, onretry = undefined }: { error: unknown; onretry?: () => void } = $props();

	const message = $derived.by(() => {
		if (!(error instanceof ApiError)) {
			return error instanceof Error ? error.message : t('error.unexpected');
		}
		if (error.status === 0) return t('error.network');
		if (error.status === 403) return t('error.forbidden');
		if (error.status === 404) return t('error.notFound');
		return error.problem.detail || error.problem.title;
	});
</script>

<div
	role="alert"
	class="flex flex-wrap items-center justify-between gap-3 rounded-md px-4 py-3 text-sm"
	style="background-color: var(--color-down-soft)"
>
	<p>{message}</p>
	{#if onretry}
		<Button size="sm" onclick={onretry}>{t('common.retry')}</Button>
	{/if}
</div>
