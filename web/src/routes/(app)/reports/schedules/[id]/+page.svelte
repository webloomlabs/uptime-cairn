<script lang="ts">
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import type { ReportSchedule } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { formatAbsolute } from '$lib/format';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Button from '$lib/components/Button.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import ReportScheduleForm from '$lib/components/ReportScheduleForm.svelte';
	import { session } from '$lib/session.svelte';

	let schedule = $state<ReportSchedule | null>(null);
	let loading = $state(true);
	let error = $state<unknown>(null);
	let confirming = $state(false);

	async function load() {
		loading = true;
		error = null;
		try {
			schedule = await api.get<ReportSchedule>(`/report-schedules/${page.params.id}`);
		} catch (caught) {
			error = caught;
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		untrack(() => void load());
	});

	async function remove() {
		await api.delete(`/report-schedules/${page.params.id}`);
		goto('/reports/schedules');
	}
</script>

<PageTitle title={schedule?.name ?? t('common.loading')}>
	{#snippet actions()}
		<Button href="/reports/schedules">{t('common.back')}</Button>
	{/snippet}
</PageTitle>

{#if loading}
	<Spinner />
{:else if error}
	<ErrorBox {error} onretry={load} />
{:else if schedule}
	<!--
		The firing time above the form, because it is the answer to the question
		somebody opened this page with and it is a fact the server computed rather
		than one this page inferred.
	-->
	<p class="muted mb-5 text-sm">
		{#if schedule.next_run_at}
			{t('schedules.nextRun', { when: formatAbsolute(schedule.next_run_at) })}
			{#if !schedule.enabled}
				· {t('schedules.pausedNote')}
			{/if}
		{:else}
			{t('schedules.noNextRun')}
		{/if}
	</p>

	{#key schedule.id}
		<ReportScheduleForm {schedule} onsaved={(saved) => (schedule = saved)} />
	{/key}

	{#if session.allows('reports:write')}
		<div class="card mt-8 p-5">
			<h2 class="text-sm font-medium">{t('schedules.delete')}</h2>
			<!--
				Said plainly, because the opposite is what people expect. The runs
				this schedule produced are kept: a run is a record of what a client
				was sent, and it outlives the arrangement that sent it.
			-->
			<p class="muted mt-1 text-sm">{t('schedules.deleteHint')}</p>
			<div class="mt-4 flex gap-2">
				{#if confirming}
					<Button variant="danger" onclick={remove}>{t('common.delete')}</Button>
					<Button onclick={() => (confirming = false)}>{t('common.cancel')}</Button>
				{:else}
					<Button variant="danger" onclick={() => (confirming = true)}>{t('common.delete')}</Button>
				{/if}
			</div>
		</div>
	{/if}
{/if}
