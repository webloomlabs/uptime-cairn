<script lang="ts">
	import { page } from '$app/state';
	import { t } from '$lib/i18n/index.svelte';
	import Button from '$lib/components/Button.svelte';
</script>

<div class="flex min-h-full items-center justify-center p-6">
	<div class="max-w-md space-y-4 text-center">
		<p class="muted text-5xl font-semibold">{page.status}</p>
		<h1 class="text-xl font-semibold">
			{page.status === 404 ? t('error.notFound') : t('error.title')}
		</h1>
		{#if page.error?.message && page.status !== 404}
			<p class="muted text-sm">{page.error.message}</p>
		{/if}
		<Button href="/" variant="primary">{t('nav.dashboard')}</Button>
	</div>
</div>
