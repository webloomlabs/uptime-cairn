<script lang="ts">
	import { goto } from '$app/navigation';
	import { t } from '$lib/i18n/index.svelte';
	import Button from '$lib/components/Button.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import ReportTemplateForm from '$lib/components/ReportTemplateForm.svelte';
</script>

<PageTitle title={t('reports.new')}>
	{#snippet actions()}
		<Button href="/reports">{t('common.cancel')}</Button>
	{/snippet}
</PageTitle>

<ReportTemplateForm onsaved={(saved) => goto(`/reports/${saved.id}`)} />
