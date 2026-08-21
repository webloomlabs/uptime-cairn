<script lang="ts">
	import type { Snippet } from 'svelte';

	/**
	 * A page heading, with the trailing accent dot the product is styled around.
	 *
	 * The dot is decorative and sits inside the heading, so it would otherwise be
	 * read out as a full stop by a screen reader on a heading that has no
	 * sentence. It is marked aria-hidden and the accessible name is the text
	 * alone.
	 */
	let { title, actions }: { title: string; actions?: Snippet } = $props();
</script>

<div class="mb-6 flex flex-wrap items-center justify-between gap-3">
	<h1 class="text-3xl font-bold tracking-tight">
		{title}<span style="color: var(--color-up)" aria-hidden="true">.</span>
	</h1>
	{#if actions}
		<div class="flex flex-wrap items-center gap-2">{@render actions()}</div>
	{/if}
</div>
