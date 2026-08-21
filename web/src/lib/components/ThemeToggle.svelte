<script lang="ts">
	import { theme } from '$lib/theme.svelte';
	import { t } from '$lib/i18n/index.svelte';

	const label = $derived(t(`theme.${theme.preference}`));
</script>

<button
	type="button"
	class="rounded-md p-2 hover:bg-[var(--surface-sunken)]"
	onclick={() => theme.cycle()}
	title={t('theme.switch', { mode: label })}
	aria-label={t('theme.switch', { mode: label })}
>
	{#if theme.preference === 'light'}
		<svg
			width="18"
			height="18"
			viewBox="0 0 24 24"
			fill="none"
			stroke="currentColor"
			stroke-width="2"
			aria-hidden="true"
		>
			<circle cx="12" cy="12" r="4" />
			<path
				d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"
			/>
		</svg>
	{:else if theme.preference === 'dark'}
		<svg
			width="18"
			height="18"
			viewBox="0 0 24 24"
			fill="none"
			stroke="currentColor"
			stroke-width="2"
			aria-hidden="true"
		>
			<path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8Z" />
		</svg>
	{:else}
		<svg
			width="18"
			height="18"
			viewBox="0 0 24 24"
			fill="none"
			stroke="currentColor"
			stroke-width="2"
			aria-hidden="true"
		>
			<rect x="2" y="4" width="20" height="14" rx="2" />
			<path d="M8 21h8M12 18v3" />
		</svg>
	{/if}
</button>
