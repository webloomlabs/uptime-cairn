<script lang="ts">
	import { statusLabel } from '$lib/format';

	/**
	 * The round status marker: a filled disc carrying a glyph.
	 *
	 * The glyph is the point. Roughly one in twelve men cannot reliably tell the
	 * up green from the down red, and this marker is the single most repeated
	 * element in the product — so up is a triangle pointing up, down points down,
	 * paused is two bars, and the colour is confirmation rather than the message.
	 * The accessible name is the status word, always.
	 */
	let { status, size = 28 }: { status: string | null | undefined; size?: number } = $props();

	const tone = $derived(
		(
			{
				up: 'up',
				down: 'down',
				pending: 'pending',
				paused: 'paused',
				maintenance: 'maintenance'
			} as Record<string, string>
		)[status ?? ''] ?? 'unknown'
	);

	const label = $derived(statusLabel(status));
	const glyph = $derived(size * 0.42);
</script>

<span
	class="inline-flex shrink-0 items-center justify-center rounded-full"
	style="width: {size}px; height: {size}px; background-color: var(--color-{tone})"
	role="img"
	aria-label={label}
	title={label}
>
	<svg width={glyph} height={glyph} viewBox="0 0 12 12" fill="#0d1017" aria-hidden="true">
		{#if status === 'down'}
			<path d="M6 10 1 3h10L6 10Z" />
		{:else if status === 'paused'}
			<rect x="2.5" y="2" width="2.5" height="8" rx="0.6" />
			<rect x="7" y="2" width="2.5" height="8" rx="0.6" />
		{:else if status === 'pending'}
			<circle cx="6" cy="6" r="2.4" />
		{:else if status === 'maintenance'}
			<rect x="1.5" y="5" width="9" height="2.2" rx="1.1" />
		{:else}
			<path d="M6 2l5 7H1l5-7Z" />
		{/if}
	</svg>
</span>
