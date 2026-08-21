<script lang="ts">
	import type { Monitor } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { formatRelative, formatResponseTime, formatUptime, monitorTarget } from '$lib/format';
	import StatusBadge from './StatusBadge.svelte';

	/**
	 * The monitor list itself.
	 *
	 * A list of links rather than a table: every row is one navigable thing, and
	 * a table row containing a link plus a checkbox plus a hover menu is a
	 * keyboard trap that has to be untangled with roles anyway. Selection, when
	 * the caller wants it, is a checkbox outside the link.
	 *
	 * `uptime` and `last_heartbeat` are `include=` embeds and may be absent — a
	 * caller that did not ask for them still renders, with an em dash rather than
	 * a zero.
	 */
	let {
		monitors,
		selectable = false,
		selected = new Set<string>(),
		ontoggle = undefined
	}: {
		monitors: Monitor[];
		selectable?: boolean;
		selected?: Set<string>;
		ontoggle?: (id: string) => void;
	} = $props();
</script>

<ul class="surface divide-y overflow-hidden rounded-lg" style="border-color: var(--border)">
	{#each monitors as monitor (monitor.id)}
		{@const target = monitorTarget(monitor)}
		<li class="flex items-center gap-3 px-4 py-3" style="border-color: var(--border)">
			{#if selectable}
				<input
					type="checkbox"
					class="h-4 w-4 shrink-0"
					checked={selected.has(monitor.id)}
					onchange={() => ontoggle?.(monitor.id)}
					aria-label={monitor.name}
				/>
			{/if}

			<a href="/monitors/{monitor.id}" class="flex min-w-0 flex-1 items-center gap-3">
				<StatusBadge status={monitor.status} dotOnly />

				<span class="min-w-0 flex-1">
					<span class="block truncate font-medium">{monitor.name}</span>
					<span class="muted block truncate text-xs">
						{monitor.type}{target ? ` · ${target}` : ''}
					</span>
				</span>

				<span class="muted hidden w-24 shrink-0 text-right text-xs sm:block">
					{#if monitor.uptime}
						<span class="block tabular-nums">{formatUptime(monitor.uptime['24h'])}</span>
						<span class="block">{t('monitors.uptime24h')}</span>
					{/if}
				</span>

				<span class="muted hidden w-20 shrink-0 text-right text-xs tabular-nums md:block">
					{formatResponseTime(monitor.last_heartbeat?.response_time_ms)}
				</span>

				<span class="muted hidden w-24 shrink-0 text-right text-xs lg:block">
					{formatRelative(monitor.last_check_at)}
				</span>

				{#if !monitor.enabled}
					<span class="muted shrink-0 text-xs">{t('status.paused')}</span>
				{/if}
			</a>
		</li>
	{/each}
</ul>
