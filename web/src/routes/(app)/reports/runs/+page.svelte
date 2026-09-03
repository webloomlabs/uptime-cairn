<script lang="ts">
	import { untrack } from 'svelte';
	import { api } from '$lib/api';
	import type { Page as ApiPage, ReportRun, ReportRunState, ReportTemplate } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { formatAbsolute } from '$lib/format';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';

	/**
	 * Run history.
	 *
	 * Three things this screen exists to make legible, each of which is invisible
	 * if the run is rendered as a single status word:
	 *
	 *   - **`partial` is a real state.** One format produced and another not.
	 *     Collapsing it into "succeeded" is how somebody concludes a delivery
	 *     went out whole; collapsing it into "failed" hides three good documents.
	 *   - **`late` is a fact about the schedule, not about the run.** The report
	 *     is fine; it arrived after it was due because the instance was down.
	 *   - **An `expired` artifact is a tombstone**, not a missing file. The bytes
	 *     were reclaimed by retention and the record of what was sent is kept.
	 *
	 * The delivery log is shown per run rather than on its own screen, because
	 * "did the client actually get it?" is asked while looking at the run that
	 * produced it.
	 */
	let runs = $state<ReportRun[]>([]);
	let names = $state<Record<string, string>>({});
	let loading = $state(true);
	let error = $state<unknown>(null);
	let stateFilter = $state<ReportRunState | ''>('');
	let expanded = $state<string | null>(null);

	/**
	 * The delivery log comes from the **single-run read**, not from the list.
	 *
	 * The server keeps it off the list on purpose, exactly as it keeps an
	 * incident's timeline off a page of incidents: fifty runs each carrying their
	 * deliveries is a query fan-out per row for a panel nobody has opened. So the
	 * row is expanded first and read second, which is also the only moment
	 * somebody has asked the question.
	 */
	let detail = $state<Record<string, ReportRun>>({});

	async function expand(run: ReportRun) {
		if (expanded === run.id) {
			expanded = null;
			return;
		}
		expanded = run.id;
		if (detail[run.id]) return;
		try {
			detail[run.id] = await api.get<ReportRun>(`/report-runs/${run.id}`);
		} catch {
			// The list row is already a usable answer — state, period, artifacts —
			// so a failed detail read degrades to showing that rather than to an
			// error where a panel should be.
			detail[run.id] = run;
		}
	}

	async function load() {
		loading = true;
		error = null;
		try {
			const query = stateFilter ? `&state=${stateFilter}` : '';
			const [runPage, templatePage] = await Promise.all([
				api.get<ApiPage<ReportRun>>(`/report-runs?limit=50${query}`),
				api
					.get<ApiPage<ReportTemplate>>('/report-templates?limit=200')
					.catch(() => ({ data: [] as ReportTemplate[] }))
			]);
			runs = runPage.data;
			names = Object.fromEntries(templatePage.data.map((tpl) => [tpl.id, tpl.name]));
		} catch (caught) {
			error = caught;
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		// Re-runs when the filter changes; `runs` is written inside `load` and is
		// deliberately not read here, so this does not loop.
		stateFilter;
		untrack(() => void load());
	});

	const STATE_TONE: Record<ReportRunState, string> = {
		queued: 'paused',
		running: 'pending',
		succeeded: 'up',
		partial: 'pending',
		failed: 'down'
	};

	/**
	 * Only a rendered artifact is downloadable.
	 *
	 * An expired one answers 410 and a failed one answers 409 — both with a
	 * reason — so offering the link and letting the server refuse would be a
	 * broken link with an explanation behind it. The explanation belongs beside
	 * the row instead.
	 */
	function downloadable(state: string): boolean {
		return state === 'rendered';
	}

	function sizeOf(bytes: number | null): string {
		if (bytes === null) return '';
		if (bytes < 1024) return `${bytes} B`;
		if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
		return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
	}

	const DELIVERY_TONE: Record<string, string> = {
		succeeded: 'up',
		failed: 'down',
		// **Not a failure**, and it must not be coloured as one: no relay
		// configured, or nothing rendered in a format this target takes.
		skipped: 'paused'
	};
</script>

<PageTitle title={t('reports.runs')} />

<div class="mb-4 flex flex-wrap items-center gap-2">
	<label class="flex items-center gap-2 text-sm">
		<span class="muted">{t('runs.state')}</span>
		<select class="field w-auto" bind:value={stateFilter}>
			<option value="">{t('common.all')}</option>
			{#each ['queued', 'running', 'succeeded', 'partial', 'failed'] as option (option)}
				<option value={option}>{t(`runs.state.${option}`)}</option>
			{/each}
		</select>
	</label>
</div>

{#if loading}
	<Spinner />
{:else if error}
	<ErrorBox {error} onretry={load} />
{:else if runs.length === 0}
	<div class="card px-4 py-14 text-center">
		<p class="font-medium">{t('runs.empty')}</p>
		<p class="muted mt-1 text-sm">{t('runs.emptyHint')}</p>
	</div>
{:else}
	<div class="card divide-y" style="border-color: var(--border)">
		{#each runs as run (run.id)}
			{@const open = expanded === run.id}
			<div style="border-color: var(--border)">
				<button
					type="button"
					class="flex w-full flex-wrap items-center gap-x-4 gap-y-2 px-4 py-3.5 text-left"
					aria-expanded={open}
					onclick={() => expand(run)}
				>
					<span
						class="inline-flex min-w-24 items-center gap-1.5 text-sm font-medium"
						style="color: var(--color-{STATE_TONE[run.state]})"
					>
						<Icon name={run.state === 'succeeded' ? 'check' : 'reports'} size={15} />
						{t(`runs.state.${run.state}`)}
					</span>

					<span class="min-w-40 flex-1 truncate text-sm">
						{names[run.report_template_id] ?? run.report_template_id}
					</span>

					<span class="muted text-xs whitespace-nowrap">
						{formatAbsolute(run.period_start)} → {formatAbsolute(run.period_end)}
						<span class="ml-1">({run.timezone})</span>
					</span>

					{#if run.late}
						<span
							class="rounded px-1.5 py-0.5 text-xs font-medium"
							style="background-color: var(--color-pending-soft)"
							title={t('runs.lateHint')}
						>
							{t('runs.late')}
						</span>
					{/if}

					<span class="muted text-xs whitespace-nowrap">{formatAbsolute(run.created_at)}</span>
					<Icon name={open ? 'chevronDown' : 'chevronLeft'} size={15} />
				</button>

				{#if open}
					{@const full = detail[run.id] ?? run}
					<div class="space-y-4 px-4 pt-1 pb-4">
						{#if run.error}
							<p
								class="text-sm"
								style="color: var(--color-{run.state === 'partial' ? 'pending' : 'down'})"
							>
								{run.error}
							</p>
						{/if}
						{#if run.state === 'partial'}
							<p class="muted text-sm">{t('runs.partialHint')}</p>
						{/if}

						<div>
							<h3 class="muted mb-2 text-xs font-medium">{t('runs.artifacts')}</h3>
							<ul class="space-y-1.5">
								{#each full.artifacts as artifact (artifact.id)}
									<li class="flex flex-wrap items-center gap-2 text-sm">
										<span class="min-w-12 font-medium">{artifact.format.toUpperCase()}</span>
										{#if downloadable(artifact.state)}
											<a
												class="inline-flex items-center gap-1 hover:underline"
												href="/api/v1/report-runs/{run.id}/artifacts/{artifact.id}"
											>
												<Icon name="download" size={14} />
												{t('runs.download')}
											</a>
											<span class="muted text-xs">{sizeOf(artifact.size_bytes)}</span>
										{:else if artifact.state === 'expired'}
											<span class="muted text-xs" title={t('runs.expiredHint')}>
												{t('runs.expired')}
											</span>
										{:else}
											<span class="text-xs" style="color: var(--color-down)">
												{t('runs.failedFormat')}{artifact.error ? ` — ${artifact.error}` : ''}
											</span>
										{/if}
									</li>
								{/each}
							</ul>
						</div>

						<div>
							<h3 class="muted mb-2 text-xs font-medium">{t('runs.deliveries')}</h3>
							{#if !detail[run.id]}
								<p class="muted text-sm">{t('common.loading')}</p>
							{:else if full.deliveries.length === 0}
								<p class="muted text-sm">{t('runs.noDeliveries')}</p>
							{:else}
								<ul class="space-y-1.5">
									{#each full.deliveries as delivery, i (i)}
										<li class="flex flex-wrap items-center gap-2 text-sm">
											<span class="min-w-16 font-medium">{delivery.type}</span>
											<span style="color: var(--color-{DELIVERY_TONE[delivery.outcome]})">
												{t(`runs.delivery.${delivery.outcome}`)}
											</span>
											{#if delivery.attempts > 1}
												<span class="muted text-xs">
													{delivery.attempts}
													{t('runs.attempts')}
												</span>
											{/if}
											<!--
												The last error is on the row rather than in a log, which
												is the same rule notification channels follow: a
												delivery that quietly stopped working is the failure
												mode this feature cannot have.
											-->
											{#if delivery.error}
												<span class="muted text-xs">— {delivery.error}</span>
											{/if}
										</li>
									{/each}
								</ul>
							{/if}
						</div>
					</div>
				{/if}
			</div>
		{/each}
	</div>
{/if}
