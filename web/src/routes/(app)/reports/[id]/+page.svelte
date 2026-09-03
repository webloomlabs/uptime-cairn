<script lang="ts">
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import type { ReportTemplate } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Button from '$lib/components/Button.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import ReportTemplateForm from '$lib/components/ReportTemplateForm.svelte';
	import { session } from '$lib/session.svelte';

	let template = $state<ReportTemplate | null>(null);
	let loading = $state(true);
	let error = $state<unknown>(null);
	let confirming = $state(false);

	async function load() {
		loading = true;
		error = null;
		try {
			template = await api.get<ReportTemplate>(`/report-templates/${page.params.id}`);
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
		await api.delete(`/report-templates/${page.params.id}`);
		goto('/reports');
	}
</script>

<PageTitle title={template?.name ?? t('common.loading')}>
	{#snippet actions()}
		<Button href="/reports">{t('common.back')}</Button>
	{/snippet}
</PageTitle>

{#if loading}
	<Spinner />
{:else if error}
	<ErrorBox {error} onretry={load} />
{:else if template}
	{#key template.id}
		<ReportTemplateForm {template} onsaved={(saved) => (template = saved)} />
	{/key}

	{#if session.allows('reports:write')}
		<div class="card mt-8 p-5">
			<h2 class="text-sm font-medium">{t('reports.delete')}</h2>
			<!--
				Said plainly, because the opposite is what people expect. Deleting a
				definition normally deletes what it made; here the runs are kept,
				because a run is a record of what a client was sent and it outlives
				the arrangement that sent it.
			-->
			<p class="muted mt-1 text-sm">{t('reports.deleteHint')}</p>
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
