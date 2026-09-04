<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { t } from '$lib/i18n/index.svelte';
	import Button from '$lib/components/Button.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import ReportScheduleForm from '$lib/components/ReportScheduleForm.svelte';

	// Pre-selects the template when arriving from one, so "schedule this report"
	// is one click rather than a form that asks which report you meant.
	const templateID = $derived(page.url.searchParams.get('template') ?? undefined);
</script>

<PageTitle title={t('schedules.new')}>
	{#snippet actions()}
		<Button href="/reports/schedules">{t('common.cancel')}</Button>
	{/snippet}
</PageTitle>

<ReportScheduleForm {templateID} onsaved={() => goto('/reports/schedules')} />
