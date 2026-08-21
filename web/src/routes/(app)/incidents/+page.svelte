<script lang="ts">
	import { untrack } from 'svelte';
	import { api } from '$lib/api';
	import type { Page as ApiPage } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { formatAbsolute, formatDuration } from '$lib/format';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Button from '$lib/components/Button.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';

	type Incident = {
		id: string;
		title: string;
		state: string;
		impact: string;
		started_at: string;
		resolved_at: string | null;
		monitor_ids: string[];
		status_page_ids: string[];
		auto_opened: boolean;
	};

	let incidents = $state<Incident[]>([]);
	let loading = $state(true);
	let error = $state<unknown>(null);
	let search = $state('');
	let onlyOpen = $state(false);

	async function load() {
		loading = true;
		error = null;
		try {
			incidents = (await api.get<ApiPage<Incident>>('/incidents?limit=100')).data;
		} catch (caught) {
			error = caught;
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		untrack(() => void load());
	});

	// Filtered in the client, deliberately and unlike the monitor list: incidents
	// are bounded by how often things break rather than by how much is monitored,
	// so a hundred of them is a busy quarter rather than a page of five thousand.
	const shown = $derived(
		incidents.filter((incident) => {
			if (onlyOpen && incident.resolved_at) return false;
			if (!search) return true;
			return incident.title.toLowerCase().includes(search.toLowerCase());
		})
	);

	function duration(incident: Incident): string {
		const end = incident.resolved_at ? Date.parse(incident.resolved_at) : Date.now();
		const seconds = Math.max(0, Math.round((end - Date.parse(incident.started_at)) / 1000));
		return formatDuration(seconds);
	}

	const IMPACT_TONE: Record<string, string> = {
		critical: 'down',
		major: 'down',
		minor: 'pending',
		none: 'paused'
	};
</script>

<PageTitle title={t('nav.incidents')} />

<div class="mb-4 flex flex-wrap items-center gap-2">
	<label class="relative min-w-48 flex-1">
		<span class="sr-only">{t('common.search')}</span>
		<span class="muted pointer-events-none absolute top-2.5 left-3">
			<Icon name="search" size={16} />
		</span>
		<input type="search" class="field pl-9" bind:value={search} placeholder={t('common.search')} />
	</label>
	<button
		type="button"
		class="card flex shrink-0 items-center gap-2 px-3 py-2 text-sm"
		style="border-radius: 0.5rem; {onlyOpen ? 'border-color: var(--accent)' : ''}"
		aria-pressed={onlyOpen}
		onclick={() => (onlyOpen = !onlyOpen)}
	>
		<Icon name="filter" size={16} />
		{t('incidents.onlyOpen')}
	</button>
</div>

{#if loading}
	<Spinner />
{:else if error}
	<ErrorBox {error} onretry={load} />
{:else if shown.length === 0}
	<div class="card px-4 py-14 text-center">
		<p class="font-medium">{t('incidents.empty')}</p>
		<p class="muted mt-1 text-sm">{t('incidents.emptyHint')}</p>
	</div>
{:else}
	<div class="card overflow-x-auto">
		<table class="w-full text-sm">
			<thead>
				<tr class="border-b text-left" style="border-color: var(--border)">
					{#each [t('incidents.status'), t('incidents.title'), t('incidents.impact'), t('incidents.started'), t('incidents.resolved'), t('incidents.duration'), t('incidents.pages')] as heading (heading)}
						<th class="muted px-4 py-3 text-xs font-medium whitespace-nowrap">{heading}</th>
					{/each}
				</tr>
			</thead>
			<tbody>
				{#each shown as incident (incident.id)}
					{@const open = incident.resolved_at === null}
					<tr class="border-b last:border-0" style="border-color: var(--border)">
						<td class="px-4 py-3.5 whitespace-nowrap">
							<span
								class="inline-flex items-center gap-1.5 font-medium"
								style="color: var(--color-{open ? 'down' : 'up'})"
							>
								<Icon name={open ? 'incidents' : 'check'} size={15} />
								{t(`incidents.state.${incident.state}`)}
							</span>
						</td>
						<td class="max-w-xs px-4 py-3.5">
							<span class="block truncate" title={incident.title}>{incident.title}</span>
							{#if incident.auto_opened}
								<span class="muted text-xs">{t('incidents.autoOpened')}</span>
							{/if}
						</td>
						<td class="px-4 py-3.5 whitespace-nowrap">
							<span
								class="rounded px-2 py-0.5 text-xs font-medium"
								style="background-color: var(--color-{IMPACT_TONE[incident.impact] ??
									'unknown'}-soft)"
							>
								{t(`incidents.impact.${incident.impact}`)}
							</span>
						</td>
						<td class="muted px-4 py-3.5 whitespace-nowrap">
							{formatAbsolute(incident.started_at)}
						</td>
						<td class="muted px-4 py-3.5 whitespace-nowrap">
							{incident.resolved_at ? formatAbsolute(incident.resolved_at) : '—'}
						</td>
						<td class="px-4 py-3.5 tabular-nums whitespace-nowrap">{duration(incident)}</td>
						<td class="muted px-4 py-3.5 tabular-nums whitespace-nowrap">
							{incident.status_page_ids.length}
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
{/if}
