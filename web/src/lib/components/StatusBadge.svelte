<script lang="ts">
	import { statusLabel } from '$lib/format';

	/**
	 * A monitor status, as a colour and a word.
	 *
	 * Never colour alone: every badge carries its label, and the dot-only variant
	 * carries the label in `title` and to assistive technology. Roughly one in
	 * twelve men cannot reliably distinguish the up green from the down red, and
	 * "is it green?" is the single most common question asked of this UI.
	 */
	let {
		status,
		size = 'md',
		dotOnly = false
	}: { status: string | null | undefined; size?: 'sm' | 'md'; dotOnly?: boolean } = $props();

	const tone = $derived(
		(
			{
				up: 'up',
				down: 'down',
				pending: 'pending',
				paused: 'paused',
				maintenance: 'maintenance',
				degraded: 'pending'
			} as Record<string, string>
		)[status ?? ''] ?? 'unknown'
	);

	const label = $derived(statusLabel(status));
</script>

{#if dotOnly}
	<span class="inline-flex items-center" title={label}>
		<span
			class="inline-block rounded-full {size === 'sm' ? 'h-2 w-2' : 'h-2.5 w-2.5'}"
			style="background-color: var(--color-{tone})"
			aria-hidden="true"
		></span>
		<span class="sr-only">{label}</span>
	</span>
{:else}
	<span
		class="inline-flex items-center gap-1.5 rounded-full font-medium whitespace-nowrap {size ===
		'sm'
			? 'px-2 py-0.5 text-xs'
			: 'px-2.5 py-1 text-sm'}"
		style="background-color: var(--color-{tone}-soft)"
	>
		<span
			class="inline-block h-1.5 w-1.5 rounded-full"
			style="background-color: var(--color-{tone})"
			aria-hidden="true"
		></span>
		{label}
	</span>
{/if}
