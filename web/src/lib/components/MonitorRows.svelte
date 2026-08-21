<script lang="ts">
	import type { Monitor } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { formatDuration, formatRelative, formatUptime, monitorTarget } from '$lib/format';
	import { typeChip } from '$lib/monitortypes';
	import StatusDial from './StatusDial.svelte';
	import Icon from './Icon.svelte';

	/**
	 * The monitor list.
	 *
	 * A list of rows rather than a table: every row is one navigable thing, and a
	 * table row containing a link plus a checkbox plus a menu is a keyboard trap
	 * that has to be untangled with roles anyway. Selection, when the caller wants
	 * it, is a checkbox outside the link.
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

	/** "Up 3 days" — the reference's most useful column, and it is derivable. */
	function since(monitor: Monitor): string {
		if (!monitor.enabled) return t('status.paused');

		const beat = monitor.last_heartbeat;
		// A monitor created moments ago has a status and no heartbeat yet. Its own
		// status is the honest answer; "Never" reads as a monitor that has failed
		// to run rather than one whose first check has not come round.
		if (!beat) return t(`status.${monitor.status}`);

		return t('monitors.upFor', {
			status: t(`status.${monitor.status}`),
			duration: formatRelative(beat.time).replace(/\s*ago$/, '')
		});
	}
</script>

<ul class="space-y-2">
	{#each monitors as monitor (monitor.id)}
		{@const target = monitorTarget(monitor)}
		<li
			class="card flex items-center gap-3 px-4 py-3 transition-colors hover:bg-[var(--surface-hover)] sm:gap-4 sm:px-5"
		>
			{#if selectable}
				<input
					type="checkbox"
					class="h-4 w-4 shrink-0 accent-[var(--accent)]"
					checked={selected.has(monitor.id)}
					onchange={() => ontoggle?.(monitor.id)}
					aria-label={monitor.name}
				/>
			{/if}

			<a href="/monitors/{monitor.id}" class="flex min-w-0 flex-1 items-center gap-3 sm:gap-4">
				<StatusDial status={monitor.enabled ? monitor.status : 'paused'} size={30} />

				<span class="min-w-0 flex-1">
					<span class="block truncate font-medium">{monitor.name}</span>
					<span class="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-1">
						<span
							class="rounded px-1.5 py-0.5 text-[11px] font-medium tracking-wide uppercase"
							style="background-color: var(--surface-sunken); color: var(--text-muted)"
						>
							{typeChip(monitor.type)}
						</span>
						<span class="muted truncate text-xs">
							{since(monitor)}{target ? ` · ${target}` : ''}
						</span>
					</span>
				</span>

				<span class="muted hidden shrink-0 items-center gap-1.5 text-xs sm:flex">
					<Icon name="refresh" size={13} />
					{formatDuration(monitor.interval_seconds)}
				</span>

				<!-- The reference draws a strip of recent checks here. This does not,
				     and the reason is in the API rather than in the styling: the list
				     endpoint embeds the *last* heartbeat and there is no embed for a
				     run of them, so a real strip would be one request per row — the
				     precise fan-out both ADR-004 and the include= design exist to
				     prevent. Drawing one from a single beat, or from an uptime ratio,
				     would be inventing a history the client has not been told. So the
				     row shows the two figures it genuinely has, and the strip lives on
				     the detail page where the heartbeats are actually fetched. -->
				<span class="hidden shrink-0 items-center gap-5 text-right md:flex">
					<span class="w-16">
						<span class="block text-sm font-semibold tabular-nums">
							{formatUptime(monitor.uptime?.['24h'])}
						</span>
						<span class="muted block text-[11px]">{t('monitors.uptime24h')}</span>
					</span>
					<span class="w-16">
						<span class="muted block text-sm tabular-nums">
							{formatUptime(monitor.uptime?.['30d'])}
						</span>
						<span class="muted block text-[11px]">{t('monitors.uptime30d')}</span>
					</span>
				</span>
			</a>

			<span class="muted shrink-0"><Icon name="dots" size={18} /></span>
		</li>
	{/each}
</ul>
