<script lang="ts">
	import { untrack } from 'svelte';
	import { api } from '$lib/api';
	import type { Page as ApiPage, ReportSchedule, ReportTemplate } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { formatAbsolute } from '$lib/format';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Button from '$lib/components/Button.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import { session } from '$lib/session.svelte';

	/**
	 * Schedules: what actually sends a report to a client.
	 *
	 * # The column that earns its place is "next"
	 *
	 * A schedule is configuration nobody looks at again until something has gone
	 * wrong, and the question people arrive with is always the same — *is this
	 * going to fire, and when?* The server computes `next_run_at` on every write
	 * and refuses a schedule that would never fire at all, so the answer is a
	 * stored fact rather than a guess this page makes.
	 *
	 * A disabled schedule still shows its firing time, greyed. "It would have
	 * gone out on the 1st, and it is switched off" is the sentence somebody needs;
	 * a blank cell would read as a schedule that is broken rather than paused.
	 */
	let schedules = $state<ReportSchedule[]>([]);
	let names = $state<Record<string, string>>({});
	let loading = $state(true);
	let error = $state<unknown>(null);

	async function load() {
		loading = true;
		error = null;
		try {
			const [schedulePage, templatePage] = await Promise.all([
				api.get<ApiPage<ReportSchedule>>('/report-schedules?limit=100'),
				api
					.get<ApiPage<ReportTemplate>>('/report-templates?limit=200')
					.catch(() => ({ data: [] as ReportTemplate[] }))
			]);
			schedules = schedulePage.data;
			names = Object.fromEntries(templatePage.data.map((tpl) => [tpl.id, tpl.name]));
		} catch (caught) {
			error = caught;
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		untrack(() => void load());
	});

	/**
	 * A one-line description of where a schedule's output goes.
	 *
	 * Counted per type rather than listed, because a schedule with four email
	 * recipients is one email target and writing them out would make the column
	 * wrap on the row that most needs to stay readable.
	 */
	function targets(schedule: ReportSchedule): string {
		const counts = new Map<string, number>();
		for (const delivery of schedule.deliveries) {
			counts.set(delivery.type, (counts.get(delivery.type) ?? 0) + 1);
		}
		return [...counts]
			.map(([type, n]) =>
				n > 1 ? `${t(`schedules.deliveryType.${type}`)} ×${n}` : t(`schedules.deliveryType.${type}`)
			)
			.join(', ');
	}

	function cadence(schedule: ReportSchedule): string {
		if (schedule.frequency === 'cron') return schedule.cron ?? 'cron';
		return t(`schedules.frequency.${schedule.frequency}`);
	}
</script>

<PageTitle title={t('reports.schedules')}>
	{#snippet actions()}
		{#if session.allows('reports:write')}
			<Button href="/reports/schedules/new" variant="primary">
				<Icon name="plus" size={16} />
				{t('schedules.new')}
			</Button>
		{/if}
	{/snippet}
</PageTitle>

{#if loading}
	<Spinner />
{:else if error}
	<ErrorBox {error} onretry={load} />
{:else if schedules.length === 0}
	<div class="card px-4 py-14 text-center">
		<p class="font-medium">{t('schedules.empty')}</p>
		<p class="muted mx-auto mt-1 max-w-md text-sm">{t('schedules.emptyHint')}</p>
		{#if session.allows('reports:write')}
			<div class="mt-5">
				<Button href="/reports/schedules/new" variant="primary">{t('schedules.new')}</Button>
			</div>
		{/if}
	</div>
{:else}
	<div class="card overflow-x-auto">
		<table class="w-full text-sm">
			<thead>
				<tr class="border-b text-left" style="border-color: var(--border)">
					{#each [t('schedules.name'), t('schedules.template'), t('schedules.cadence'), t('schedules.deliveries'), t('schedules.next'), t('schedules.last')] as heading, i (i)}
						<th class="muted px-4 py-3 text-xs font-medium whitespace-nowrap">{heading}</th>
					{/each}
				</tr>
			</thead>
			<tbody>
				{#each schedules as schedule (schedule.id)}
					<tr class="border-b last:border-0" style="border-color: var(--border)">
						<td class="max-w-xs px-4 py-3.5">
							<a href="/reports/schedules/{schedule.id}" class="block truncate hover:underline">
								{schedule.name}
							</a>
							{#if !schedule.enabled}
								<span
									class="mt-0.5 inline-block rounded px-1.5 py-0.5 text-xs font-medium"
									style="background-color: var(--surface-hover)"
								>
									{t('schedules.paused')}
								</span>
							{/if}
						</td>
						<td class="max-w-xs px-4 py-3.5">
							<a
								href="/reports/{schedule.report_template_id}"
								class="block truncate hover:underline"
							>
								{names[schedule.report_template_id] ?? schedule.report_template_id}
							</a>
						</td>
						<td class="px-4 py-3.5 whitespace-nowrap">
							{cadence(schedule)}
							{#if schedule.frequency !== 'cron' && schedule.send_at}
								<span class="muted text-xs">· {schedule.send_at}</span>
							{/if}
							<span class="muted block text-xs">{schedule.timezone}</span>
						</td>
						<td class="muted px-4 py-3.5">{targets(schedule)}</td>
						<td class="px-4 py-3.5 whitespace-nowrap">
							{#if schedule.next_run_at}
								<span style={schedule.enabled ? '' : 'color: var(--text-muted)'}>
									{formatAbsolute(schedule.next_run_at)}
								</span>
							{:else}
								<span class="muted">—</span>
							{/if}
						</td>
						<td class="muted px-4 py-3.5 whitespace-nowrap">
							{schedule.last_run_at
								? formatAbsolute(schedule.last_run_at)
								: t('schedules.neverRun')}
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
{/if}
