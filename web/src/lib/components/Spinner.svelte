<script lang="ts">
	import { t } from '$lib/i18n/index.svelte';

	let { label = undefined }: { label?: string } = $props();
	const text = $derived(label ?? t('common.loading'));
</script>

<div class="muted flex items-center gap-2 py-8 text-sm" role="status">
	<span
		class="h-4 w-4 animate-spin rounded-full border-2 border-current border-t-transparent"
		aria-hidden="true"
	></span>
	{text}
</div>
