<script lang="ts">
	import { untrack } from 'svelte';
	import { api, ApiError } from '$lib/api';
	import type { ReportTemplate, ReportRun, Page as ApiPage } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { formatAbsolute } from '$lib/format';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Button from '$lib/components/Button.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import { session } from '$lib/session.svelte';

	let templates = $state<ReportTemplate[]>([]);
	let loading = $state(true);
	let error = $state<unknown>(null);

	/**
	 * Per-template generate state.
	 *
	 * `generate` answers 202 with a run to poll rather than the document, so the
	 * honest thing to say here is "queued" and a pointer at the run history —
	 * not a spinner that implies the file is on its way to the browser.
	 */
	let queued = $state<Record<string, string>>({});
	let refused = $state<Record<string, string>>({});

	async function load() {
		loading = true;
		error = null;
		try {
			templates = (await api.get<ApiPage<ReportTemplate>>('/report-templates?limit=100')).data;
		} catch (caught) {
			error = caught;
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		untrack(() => void load());
	});

	async function generate(template: ReportTemplate) {
		delete refused[template.id];
		try {
			// An explicit empty object, not an absent body: the endpoint's requestBody
			// is `required: true` (docs/api/openapi.yaml), so a bodyless POST is
			// refused with `malformed-json` and a detail of "EOF" before any of the
			// fields' documented defaults apply. `{}` is what asks for them: the
			// template's own period, resolved to the last completed one.
			const run = await api.post<ReportRun>(`/report-templates/${template.id}/generate`, {});
			queued[template.id] = run.id;
		} catch (caught) {
			// 501 with no worker, 503 with a full queue. Both are the server saying
			// something an operator can act on, so the sentence is shown rather
			// than swallowed into a generic failure.
			refused[template.id] = caught instanceof ApiError ? caught.message : t('common.retry');
		}
	}
</script>

<PageTitle title={t('reports.title')}>
	{#snippet actions()}
		{#if session.allows('reports:write')}
			<Button href="/reports/new" variant="primary">
				<Icon name="plus" size={16} />
				{t('reports.new')}
			</Button>
		{/if}
	{/snippet}
</PageTitle>

{#if loading}
	<Spinner />
{:else if error}
	<ErrorBox {error} onretry={load} />
{:else if templates.length === 0}
	<div class="card px-4 py-14 text-center">
		<p class="font-medium">{t('reports.empty')}</p>
		<p class="muted mx-auto mt-1 max-w-md text-sm">{t('reports.emptyHint')}</p>
		{#if session.allows('reports:write')}
			<div class="mt-5">
				<Button href="/reports/new" variant="primary">{t('reports.new')}</Button>
			</div>
		{/if}
	</div>
{:else}
	<div class="card overflow-x-auto">
		<table class="w-full text-sm">
			<thead>
				<tr class="border-b text-left" style="border-color: var(--border)">
					{#each [t('reports.name'), t('reports.type'), t('reports.period'), t('reports.formats'), t('reports.updated'), ''] as heading, i (i)}
						<th class="muted px-4 py-3 text-xs font-medium whitespace-nowrap">{heading}</th>
					{/each}
				</tr>
			</thead>
			<tbody>
				{#each templates as template (template.id)}
					<tr class="border-b last:border-0" style="border-color: var(--border)">
						<td class="max-w-xs px-4 py-3.5">
							<a href="/reports/{template.id}" class="block truncate hover:underline">
								{template.name}
							</a>
							{#if template.description}
								<span class="muted block truncate text-xs">{template.description}</span>
							{/if}
						</td>
						<td class="px-4 py-3.5 whitespace-nowrap">
							{t(`reports.type.${template.type}`)}
						</td>
						<td class="muted px-4 py-3.5 whitespace-nowrap">
							{t(`reports.period.${template.period}`)}
							<span class="text-xs"
								>· {t(`reports.periodStyle.${template.period_style}`).split(' —')[0]}</span
							>
						</td>
						<td class="px-4 py-3.5">
							<span class="flex flex-wrap gap-1">
								{#each template.formats as format (format)}
									<span
										class="rounded px-1.5 py-0.5 text-xs font-medium"
										style="background-color: var(--surface-hover)">{format.toUpperCase()}</span
									>
								{/each}
							</span>
						</td>
						<td class="muted px-4 py-3.5 whitespace-nowrap">
							{formatAbsolute(template.updated_at)}
						</td>
						<td class="px-4 py-3.5 text-right whitespace-nowrap">
							{#if refused[template.id]}
								<span class="text-xs" style="color: var(--color-down)">{refused[template.id]}</span>
							{:else if queued[template.id]}
								<a
									href="/reports/runs"
									class="text-xs hover:underline"
									style="color: var(--color-up)">{t('reports.generated')}</a
								>
							{:else if session.allows('reports:write')}
								<button
									type="button"
									class="text-sm hover:underline"
									onclick={() => generate(template)}
								>
									{t('reports.generate')}
								</button>
							{/if}
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
{/if}
