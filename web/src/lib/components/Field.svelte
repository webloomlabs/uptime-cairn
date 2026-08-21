<script lang="ts">
	import type { Snippet } from 'svelte';
	import { t } from '$lib/i18n/index.svelte';

	/**
	 * A labelled form control with its own error slot.
	 *
	 * `error` is fed from the server's RFC 9457 `errors[]` array, keyed by JSON
	 * pointer — so a field highlights because the server named it, not because
	 * the client string-matched a sentence. `aria-describedby` and
	 * `aria-invalid` are wired here once so no individual form has to remember.
	 */
	let {
		label,
		id,
		error = undefined,
		hint = undefined,
		optional = false,
		children
	}: {
		label: string;
		id: string;
		error?: string;
		hint?: string;
		optional?: boolean;
		children: Snippet<[{ id: string; describedBy: string | undefined; invalid: boolean }]>;
	} = $props();

	const hintId = $derived(hint ? `${id}-hint` : undefined);
	const errorId = $derived(error ? `${id}-error` : undefined);
	const describedBy = $derived([errorId, hintId].filter(Boolean).join(' ') || undefined);
</script>

<div class="space-y-1.5">
	<label for={id} class="block text-sm font-medium">
		{label}
		{#if optional}
			<span class="muted font-normal">({t('common.optional')})</span>
		{/if}
	</label>
	{@render children({ id, describedBy, invalid: Boolean(error) })}
	{#if error}
		<p id={errorId} class="text-sm" style="color: var(--color-down)">{error}</p>
	{:else if hint}
		<p id={hintId} class="muted text-sm">{hint}</p>
	{/if}
</div>
