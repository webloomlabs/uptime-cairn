<script lang="ts">
	import { api } from '$lib/api';
	import type { Monitor, Page as ApiPage } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';

	/**
	 * Picks one monitor by searching, rather than by choosing from a list.
	 *
	 * A `<select>` of every monitor is the obvious control and it is the one
	 * ADR-004 rules out: it ships the whole collection to the browser, which is
	 * the exact mechanism this project exists to avoid. So the search is a
	 * server-side query, bounded to a handful of rows, and an install with five
	 * thousand monitors pays the same as one with five.
	 *
	 * Monitors already placed on the page are filtered out here rather than
	 * refused on save, because the server's rule — a monitor appears in at most
	 * one section per page — is easier to obey than to recover from.
	 */
	let {
		exclude,
		onpick,
		id
	}: {
		exclude: Set<string>;
		onpick: (monitor: { id: string; name: string }) => void;
		id: string;
	} = $props();

	let query = $state('');
	let results = $state<Monitor[]>([]);
	let searching = $state(false);
	let open = $state(false);
	let failed = $state(false);

	// One in-flight search at a time. Typing fast otherwise races, and the
	// answer to a prefix can land after the answer to the whole word.
	let inFlight: AbortController | null = null;
	let timer: ReturnType<typeof setTimeout> | null = null;

	const visible = $derived(results.filter((monitor) => !exclude.has(monitor.id)));

	function schedule(value: string) {
		query = value;
		open = true;
		failed = false;
		if (timer) clearTimeout(timer);
		if (value.trim() === '') {
			inFlight?.abort();
			results = [];
			searching = false;
			return;
		}
		searching = true;
		timer = setTimeout(() => void run(value), 250);
	}

	async function run(value: string) {
		inFlight?.abort();
		const controller = new AbortController();
		inFlight = controller;
		try {
			const page = await api.get<ApiPage<Monitor>>('/monitors', {
				query: { search: value, limit: 10 },
				signal: controller.signal
			});
			if (controller.signal.aborted) return;
			results = page.data;
		} catch (caught) {
			if (caught instanceof DOMException && caught.name === 'AbortError') return;
			// A failed lookup is reported in place. Silently showing no results
			// would read as "no such monitor", which is a different fact.
			results = [];
			failed = true;
		} finally {
			if (!controller.signal.aborted) searching = false;
		}
	}

	function choose(monitor: Monitor) {
		onpick({ id: monitor.id, name: monitor.name });
		query = '';
		results = [];
		open = false;
	}
</script>

<div class="relative">
	<input
		{id}
		type="search"
		class="field"
		placeholder={t('statusPages.addMonitor')}
		value={query}
		autocomplete="off"
		role="combobox"
		aria-expanded={open && query.trim() !== ''}
		aria-controls="{id}-results"
		oninput={(e) => schedule(e.currentTarget.value)}
		onfocus={() => (open = true)}
		onblur={() => setTimeout(() => (open = false), 150)}
	/>

	{#if open && query.trim() !== ''}
		<div
			id="{id}-results"
			role="listbox"
			class="absolute z-20 mt-1 max-h-64 w-full overflow-y-auto rounded-lg border shadow-lg"
			style="border-color: var(--border); background-color: var(--surface-raised)"
		>
			{#if searching}
				<p class="muted px-3 py-2 text-sm">{t('common.loading')}</p>
			{:else if failed}
				<p class="px-3 py-2 text-sm" style="color: var(--color-down)">
					{t('statusPages.searchFailed')}
				</p>
			{:else if visible.length === 0}
				<p class="muted px-3 py-2 text-sm">{t('statusPages.noMatches')}</p>
			{:else}
				{#each visible as monitor (monitor.id)}
					<button
						type="button"
						role="option"
						aria-selected="false"
						class="block w-full px-3 py-2 text-left text-sm transition-colors hover:bg-[var(--surface-hover)]"
						onclick={() => choose(monitor)}
					>
						<span class="block truncate font-medium">{monitor.name}</span>
						<span class="muted block truncate text-xs">{monitor.type}</span>
					</button>
				{/each}
			{/if}
		</div>
	{/if}
</div>
