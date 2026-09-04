<script lang="ts">
	/**
	 * The icon set, drawn inline.
	 *
	 * Hand-written paths rather than an icon package: this is a dozen glyphs on a
	 * 24-unit grid, against a dependency that ships thousands and a component
	 * wrapper for each. They inherit `currentColor` and take their size from the
	 * caller, which is the whole contract.
	 *
	 * Every icon here is decorative — the label beside it carries the meaning — so
	 * they are hidden from assistive technology rather than given names that would
	 * be read out twice.
	 */
	let {
		name,
		size = 18,
		stroke = 1.75
	}: { name: string; size?: number; stroke?: number } = $props();

	const paths: Record<string, string> = {
		monitoring: 'M12 3a9 9 0 1 0 9 9 M12 7a5 5 0 1 0 5 5 M12 12h.01',
		incidents: 'M12 3 4 6v6c0 4.4 3.2 8.2 8 9 4.8-.8 8-4.6 8-9V6l-8-3Z M12 9v4 M12 16h.01',
		status:
			'M5 12a7 7 0 0 1 7-7 M8.5 12a3.5 3.5 0 0 1 3.5-3.5 M12 12h.01 M15.5 12a3.5 3.5 0 0 0-3.5-3.5 M19 12a7 7 0 0 0-7-7 M12 12v7',
		maintenance:
			'M14.7 6.3a4 4 0 0 0-5.4 5.4L4 17v3h3l5.3-5.3a4 4 0 0 0 5.4-5.4l-2.5 2.5-2.5-2.5 2.5-2.5Z',
		notifications: 'M18 9a6 6 0 1 0-12 0c0 5-2 7-2 7h16s-2-2-2-7 M10.5 20a2 2 0 0 0 3 0',
		settings:
			'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z M19.4 15a1.6 1.6 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.6 1.6 0 0 0-1.8-.3 1.6 1.6 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1A1.6 1.6 0 0 0 9 19.4a1.6 1.6 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.6 1.6 0 0 0 .3-1.8 1.6 1.6 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1A1.6 1.6 0 0 0 4.6 9a1.6 1.6 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.6 1.6 0 0 0 1.8.3H9a1.6 1.6 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.6 1.6 0 0 0 1 1.5 1.6 1.6 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.6 1.6 0 0 0-.3 1.8V9a1.6 1.6 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.6 1.6 0 0 0-1.5 1Z',
		search: 'M11 19a8 8 0 1 0 0-16 8 8 0 0 0 0 16Z M21 21l-4.3-4.3',
		filter: 'M3 5h18 M7 12h10 M10 19h4',
		tag: 'M3 12V5a2 2 0 0 1 2-2h7l9 9-9 9-9-9Z M7.5 7.5h.01',
		sort: 'M7 4v16 M4 17l3 3 3-3 M17 20V4 M14 7l3-3 3 3',
		plus: 'M12 5v14 M5 12h14',
		reports:
			'M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8l-5-5Z M14 3v5h5 M9 13h6 M9 17h4',
		download: 'M12 4v11 M8 11l4 4 4-4 M5 20h14',
		close: 'M6 6l12 12 M18 6 6 18',
		calendar:
			'M7 3v3 M17 3v3 M4 9h16 M5 5h14a1 1 0 0 1 1 1v13a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1Z',
		brand: 'M12 3l7 4v5c0 4.4-3 8.2-7 9-4-.8-7-4.6-7-9V7l7-4Z M9.5 12l1.8 1.8L15 10',
		refresh: 'M3 12a9 9 0 0 1 15-6.7L21 8 M21 3v5h-5 M21 12a9 9 0 0 1-15 6.7L3 16 M3 21v-5h5',
		pause: 'M10 5v14 M14 5v14',
		play: 'M7 4l12 8-12 8V4Z',
		edit: 'M12 20h9 M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4 12.5-12.5Z',
		trash:
			'M4 7h16 M9 7V5a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2 M6 7l1 13a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1l1-13',
		external: 'M14 4h6v6 M20 4l-9 9 M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5',
		chevronLeft: 'M15 6l-6 6 6 6',
		chevronDown: 'M6 9l6 6 6-6',
		check: 'M4 12.5 9 17.5 20 6.5',
		clock: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z M12 7v5l3 2',
		shield: 'M12 3 4 6v6c0 4.4 3.2 8.2 8 9 4.8-.8 8-4.6 8-9V6l-8-3Z',
		globe: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z M3 12h18 M12 3a14 14 0 0 1 0 18 14 14 0 0 1 0-18',
		users:
			'M16 20v-1a4 4 0 0 0-4-4H7a4 4 0 0 0-4 4v1 M9.5 11a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7 M21 20v-1a4 4 0 0 0-3-3.9 M16.5 4.1a4 4 0 0 1 0 7.8',
		logout: 'M9 21H5a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h4 M16 17l5-5-5-5 M21 12H9',
		dots: 'M12 6h.01 M12 12h.01 M12 18h.01',
		copy: 'M9 9h10a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1H9a1 1 0 0 1-1-1V10a1 1 0 0 1 1-1Z M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1'
	};

	const d = $derived(paths[name] ?? '');
</script>

<svg
	width={size}
	height={size}
	viewBox="0 0 24 24"
	fill="none"
	stroke="currentColor"
	stroke-width={stroke}
	stroke-linecap="round"
	stroke-linejoin="round"
	aria-hidden="true"
	class="shrink-0"
>
	{#each d.split(' M').filter(Boolean) as segment, index (index)}
		<path d={index === 0 ? segment : 'M' + segment} />
	{/each}
</svg>
