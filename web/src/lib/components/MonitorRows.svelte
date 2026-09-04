<script lang="ts">
	import type { Monitor } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { formatDuration, formatRelative, formatUptime, monitorTarget } from '$lib/format';
	import { typeChip } from '$lib/monitortypes';
	import StatusDial from './StatusDial.svelte';
	import HeartbeatStrip from './HeartbeatStrip.svelte';
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
					<!--
						Deliberately not `flex-wrap`. The text below carries a live duration
						("Up 52s" becoming "Up 1m"), so a wrapping line changes the row's
						height on a timer: every row in the list shifts under the pointer
						whenever one monitor's duration grows a character. One line that
						truncates keeps the row a fixed height no matter what the clock
						does — which is why the badge cannot shrink and the text can.
					-->
					<span class="mt-0.5 flex items-center gap-x-2">
						<span
							class="shrink-0 rounded px-1.5 py-0.5 text-[11px] font-medium tracking-wide uppercase"
							style="background-color: var(--surface-sunken); color: var(--text-muted)"
						>
							{typeChip(monitor.type)}
						</span>
						<span class="muted min-w-0 truncate text-xs">
							{since(monitor)}{target ? ` · ${target}` : ''}
						</span>
					</span>
				</span>

				<span class="muted hidden shrink-0 items-center gap-1.5 text-xs sm:flex">
					<Icon name="refresh" size={13} />
					{formatDuration(monitor.interval_seconds)}
				</span>

				<!-- The run of recent checks.
				     It is drawn from `include=heartbeats`, which the server resolves
				     for the whole page in one statement. A strip fetched per row would
				     be the exact fan-out both ADR-004 and the include= design exist to
				     prevent, and drawing one from a single beat or from an uptime
				     ratio would be inventing a history the client was never told.
				     Absent when the caller did not ask for the embed, in which case
				     the row simply has no strip rather than an empty one. -->
				{#if monitor.heartbeats?.length}
					<span class="hidden shrink-0 lg:block">
						<HeartbeatStrip beats={monitor.heartbeats} limit={30} height={20} />
					</span>
				{/if}

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
