<script lang="ts">
	import { untrack } from 'svelte';
	import { api } from '$lib/api';
	import type {
		Page as ApiPage,
		ReportArtifact,
		ReportRun,
		ReportRunState,
		ReportShareCreated,
		ReportTemplate
	} from '$lib/types';
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

	/**
	 * Share links.
	 *
	 * **The URL is held here and nowhere else.** The server returns it once, at
	 * creation, and no read path can produce it again — the token is hashed for
	 * lookup and sealed for replay, and the sealed copy is not on the wire. So
	 * this map is the only place it exists in the browser, it is not persisted,
	 * and a page reload loses it. That is the intended behaviour rather than a
	 * limitation: a screen that could re-display a live credential is a screen
	 * that leaks one the first time it is screenshotted or pasted into a ticket.
	 *
	 * Somebody who has lost the URL revokes and creates another, which is also
	 * the honest thing to do — a link you cannot produce is a link you have lost
	 * control of.
	 */
	let freshLinks = $state<Record<string, string>>({});
	let shareExpiryDays = $state<Record<string, number>>({});
	let sharing = $state<string | null>(null);
	let shareError = $state<Record<string, string>>({});
	let copied = $state<string | null>(null);

	async function createShare(run: ReportRun) {
		sharing = run.id;
		delete shareError[run.id];
		try {
			const days = shareExpiryDays[run.id] ?? 0;
			const body: Record<string, unknown> = {};
			if (days > 0) {
				body.expires_at = new Date(Date.now() + days * 86_400_000).toISOString();
			}
			const created = await api.post<ReportShareCreated>(`/report-runs/${run.id}/share`, body);
			freshLinks[run.id] = created.url;
			// Re-read so the row shows the link exists, its expiry and — later —
			// whether the client has opened it. The URL above is not in that read
			// and never will be.
			detail[run.id] = await api.get<ReportRun>(`/report-runs/${run.id}`);
		} catch (caught) {
			shareError[run.id] = caught instanceof Error ? caught.message : String(caught);
		} finally {
			sharing = null;
		}
	}

	async function revokeShare(run: ReportRun) {
		sharing = run.id;
		delete shareError[run.id];
		try {
			await api.delete(`/report-runs/${run.id}/share`);
			delete freshLinks[run.id];
			detail[run.id] = await api.get<ReportRun>(`/report-runs/${run.id}`);
		} catch (caught) {
			shareError[run.id] = caught instanceof Error ? caught.message : String(caught);
		} finally {
			sharing = null;
		}
	}

	async function copyLink(runId: string, url: string) {
		try {
			await navigator.clipboard.writeText(url);
			copied = runId;
			setTimeout(() => (copied === runId ? (copied = null) : null), 2000);
		} catch {
			// Clipboard access is refused in plenty of ordinary configurations —
			// an insecure origin, a browser policy. The URL is on screen and
			// selectable, so failing quietly leaves the operator with a working
			// path rather than an error over a convenience.
		}
	}

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
	 * Only an artifact the server offered a link for is downloadable.
	 *
	 * **Keyed on `download_url` rather than on `state`, and the difference is the
	 * whole point.** An expired one answers 410 and a failed one answers 409, both
	 * with a reason — and a *rendered* one whose file is not on disk answers 410
	 * too. That last case is a database restored without `<data-dir>/reports/`,
	 * which is the silent half of the backup procedure, and it is the one a
	 * state-based check would get wrong: the row says `rendered`, so the old check
	 * offered a link that could not work.
	 *
	 * The server does the disk check and expresses the answer by withholding the
	 * URL. Offering a link and letting the server refuse it is a broken link with
	 * an explanation behind it; the explanation belongs beside the row.
	 */
	function downloadable(artifact: ReportArtifact): boolean {
		return artifact.download_url !== null;
	}

	/**
	 * A rendered artifact with no download link: the bytes are not on disk.
	 *
	 * Distinct from `expired`, which is retention doing its job and needs no
	 * action, and from `failed`, which never produced a file at all. This one is
	 * an operator's to fix, and the hint says how.
	 */
	function unavailable(artifact: ReportArtifact): boolean {
		return artifact.state === 'rendered' && artifact.download_url === null;
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
										{#if downloadable(artifact)}
											<a
												class="inline-flex items-center gap-1 hover:underline"
												href="/api/v1/report-runs/{run.id}/artifacts/{artifact.id}"
											>
												<Icon name="download" size={14} />
												{t('runs.download')}
											</a>
											<span class="muted text-xs">{sizeOf(artifact.size_bytes)}</span>
										{:else if unavailable(artifact)}
											<!--
												The row says this rendered and the file is not there. Almost
												always a database restored without <data-dir>/reports/, so the
												hint names that rather than describing the symptom.
											-->
											<span
												class="text-xs"
												style="color: var(--color-pending)"
												title={t('runs.unavailableHint')}
											>
												{t('runs.unavailable')}
											</span>
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

										<!--
											The offsite copy, and **never coloured as though the report
											were damaged**. The mirror is a durability copy and never a
											read path, so a bucket that was briefly unreachable leaves a
											perfectly downloadable report — rendering this in the failure
											colour would send an operator looking for a problem with a
											file that is fine.
										-->
										{#if artifact.mirror}
											<span
												class="muted text-xs"
												title={artifact.mirror.state === 'failed'
													? `${t('runs.mirrorFailedHint')} ${artifact.mirror.error ?? ''}`
													: undefined}
											>
												· {t(`runs.mirror.${artifact.mirror.state}`)}
											</span>
										{/if}
									</li>
								{/each}
							</ul>
						</div>

						<!--
							Share links.
							
							Offered only on a run that produced something. A link onto a run
							with no rendered artifact resolves to a page with nothing to
							download, which is a worse thing to hand a client than no link.
						-->
						{#if full.artifacts.some((a) => a.state === 'rendered')}
							<div>
								<h3 class="muted mb-2 text-xs font-medium">{t('runs.share')}</h3>

								{#if freshLinks[run.id]}
									<!--
										Shown once. The server returns the URL at creation and no
										read path can produce it again, so this is the only moment
										it exists outside the recipient's inbox.
									-->
									<div class="rounded p-3 text-sm" style="background-color: var(--color-up-soft)">
										<p class="mb-2 font-medium">{t('runs.shareOnce')}</p>
										<div class="flex flex-wrap items-center gap-2">
											<code class="flex-1 break-all text-xs">{freshLinks[run.id]}</code>
											<button
												type="button"
												class="inline-flex items-center gap-1 text-xs hover:underline"
												onclick={() => copyLink(run.id, freshLinks[run.id])}
											>
												<Icon name="copy" size={13} />
												{copied === run.id ? t('runs.shareCopied') : t('runs.shareCopy')}
											</button>
										</div>
									</div>
								{/if}

								{#if full.share}
									<div class="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-sm">
										<span>{t('runs.shareExists')}</span>
										<span class="muted text-xs">
											{t('runs.shareCreated')}: {formatAbsolute(full.share.created_at)}
										</span>
										<span class="muted text-xs">
											{full.share.expires_at
												? `${t('runs.shareExpires')}: ${formatAbsolute(full.share.expires_at)}`
												: t('runs.shareNeverExpires')}
										</span>
										<span class="muted text-xs">
											{full.share.last_accessed_at
												? `${t('runs.shareOpened')}: ${formatAbsolute(full.share.last_accessed_at)}`
												: t('runs.shareNotOpened')}
										</span>
										<button
											type="button"
											class="text-xs hover:underline"
											style="color: var(--color-down)"
											disabled={sharing === run.id}
											onclick={() => revokeShare(run)}
										>
											{t('runs.shareRevoke')}
										</button>
									</div>
								{:else}
									<p class="muted mb-2 text-sm">{t('runs.shareHint')}</p>
									<div class="flex flex-wrap items-center gap-2">
										<label class="flex items-center gap-2 text-sm">
											<span class="muted text-xs">{t('runs.shareExpiry')}</span>
											<select class="field w-auto text-sm" bind:value={shareExpiryDays[run.id]}>
												<option value={0}>{t('runs.shareExpiryNever')}</option>
												{#each [7, 30, 90] as days (days)}
													<option value={days}>{t('runs.shareExpiryDays', { n: days })}</option>
												{/each}
											</select>
										</label>
										<button
											type="button"
											class="text-sm hover:underline"
											style="color: var(--accent)"
											disabled={sharing === run.id}
											onclick={() => createShare(run)}
										>
											{t('runs.shareCreate')}
										</button>
									</div>
								{/if}

								{#if shareError[run.id]}
									<p class="mt-2 text-sm" style="color: var(--color-down)">{shareError[run.id]}</p>
								{/if}
							</div>
						{/if}

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
