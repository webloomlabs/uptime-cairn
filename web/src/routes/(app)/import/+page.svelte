<script lang="ts">
	import { api, getCSRFToken } from '$lib/api';
	import { t } from '$lib/i18n/index.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import Button from '$lib/components/Button.svelte';
	import Field from '$lib/components/Field.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Spinner from '$lib/components/Spinner.svelte';

	/**
	 * The guided Uptime Kuma import.
	 *
	 * The same importer the CLI runs, through the same seam. What this screen
	 * adds is the part a migrating user actually needs: a dry run they can read
	 * before committing, and a report afterwards that names everything which did
	 * not come across.
	 *
	 * The report is the point of the whole feature, and it is laid out
	 * accordingly — what needs attention first and in full, the tally after it.
	 * An import summary that leads with "1,204 monitors imported" and buries
	 * thirty unsupported types below the fold is a summary that gets skimmed, and
	 * skimming it is how somebody finds out during an outage that a monitor they
	 * thought they had was never created.
	 *
	 * The upload is a plain multipart POST rather than going through the API
	 * client, which speaks JSON. It is the one place in the frontend that calls
	 * `fetch` directly, and it still uses the same credentials and the same CSRF
	 * token — this is a body-encoding exception, not a back door.
	 */
	type ImportEntry = {
		source_file: string;
		entity_type: string;
		source_id: string | null;
		source_name: string;
		result: string;
		target_id: string | null;
		detail: string | null;
	};

	type ImportSource = {
		filename: string;
		kuma_version: string | null;
		detected_entities: Record<string, number>;
	};

	type ImportSummary = {
		imported: number;
		renamed: number;
		skipped: number;
		failed: number;
		unsupported: number;
	};

	type ImportJob = {
		id: string;
		state: string;
		dry_run: boolean;
		sources: ImportSource[];
		summary: Record<string, ImportSummary>;
		entries: ImportEntry[];
		error: string | null;
		started_at: string | null;
		finished_at: string | null;
		created_at: string;
	};

	let files = $state<FileList | null>(null);
	let dryRun = $state(true);
	let conflict = $state('rename');
	let namePrefix = $state('');
	let importMonitors = $state(true);
	let importTags = $state(true);
	let importNotifications = $state(true);
	let importStatusPages = $state(true);
	let importHistory = $state(false);
	let resume = $state(false);

	let job = $state<ImportJob | null>(null);
	let running = $state(false);
	let error = $state<unknown>(null);

	const finished = $derived(job !== null && ['succeeded', 'partial', 'failed'].includes(job.state));

	/**
	 * Everything worth reading: what did not come across, plus what did but with
	 * something lost along the way.
	 */
	const attention = $derived(
		(job?.entries ?? []).filter(
			(entry) => entry.result !== 'imported' || (entry.detail ?? '') !== ''
		)
	);

	async function start(event: SubmitEvent) {
		event.preventDefault();
		if (!files || files.length === 0) {
			error = new Error(t('import.selectFiles'));
			return;
		}

		running = true;
		error = null;
		job = null;

		const form = new FormData();
		form.append(
			'options',
			JSON.stringify({
				dry_run: dryRun,
				conflict_strategy: conflict,
				name_prefix: namePrefix || null,
				import_monitors: importMonitors,
				import_tags: importTags,
				import_notifications: importNotifications,
				import_status_pages: importStatusPages,
				import_history: importHistory,
				enable_after_import: resume
			})
		);
		for (const file of files) form.append('files', file, file.name);

		try {
			const token = getCSRFToken();
			const response = await fetch('/api/v1/imports/kuma', {
				method: 'POST',
				credentials: 'same-origin',
				headers: token ? { 'X-Cairn-CSRF-Token': token } : {},
				body: form
			});
			const body = await response.json();
			if (!response.ok) {
				error = new Error(body?.detail || body?.title || t('common.error'));
				running = false;
				return;
			}
			job = body as ImportJob;
			await poll(job.id);
		} catch (caught) {
			error = caught;
			running = false;
		}
	}

	/**
	 * Polls until the job finishes.
	 *
	 * A second apart rather than tighter: an import of a Kuma install with a year
	 * of history is minutes of work, and the only thing a faster poll buys is a
	 * request per second against a server that is busy doing the import.
	 */
	async function poll(id: string) {
		for (;;) {
			await new Promise((resolve) => setTimeout(resolve, 1000));
			try {
				const next = await api.get<ImportJob>(`/imports/${id}`);
				job = next;
				if (['succeeded', 'partial', 'failed'].includes(next.state)) {
					running = false;
					return;
				}
			} catch (caught) {
				error = caught;
				running = false;
				return;
			}
		}
	}

	function reset() {
		job = null;
		error = null;
		files = null;
	}

	const RESULT_TONE: Record<string, string> = {
		imported: 'up',
		renamed: 'pending',
		skipped: 'paused',
		failed: 'down',
		unsupported: 'down'
	};
</script>

<PageTitle title={t('import.title')} />

{#if job && finished}
	<div class="max-w-3xl space-y-6">
		<section class="card p-5">
			<h2 class="font-semibold">
				{t('import.report')}<span style="color: var(--color-up)" aria-hidden="true">.</span>
			</h2>
			<p
				class="mt-2 text-sm"
				style="color: var(--color-{job.state === 'succeeded'
					? 'up'
					: job.state === 'failed'
						? 'down'
						: 'pending'})"
			>
				{t(`import.state.${job.state}`)}
			</p>
			{#if job.error}
				<p class="muted mt-1 text-sm">{job.error}</p>
			{/if}
			{#if job.dry_run}
				<p class="muted mt-1 text-sm">{t('import.dryRunHint')}</p>
			{/if}

			<dl class="mt-4 space-y-2 text-sm">
				{#each job.sources as source (source.filename)}
					<div>
						<dt class="font-medium">
							{source.filename}
							{#if source.kuma_version}
								<span class="muted font-normal">· Uptime Kuma {source.kuma_version}</span>
							{/if}
						</dt>
						<dd class="muted text-xs">
							{Object.entries(source.detected_entities)
								.map(([entity, count]) => `${count} ${entity}`)
								.join(' · ')}
						</dd>
					</div>
				{/each}
			</dl>
		</section>

		<section class="card p-5">
			<h2 class="font-semibold">{t('import.needsAttention')}</h2>
			{#if attention.length === 0}
				<p class="muted mt-3 text-sm">{t('import.nothingToReport')}</p>
			{:else}
				<ul class="mt-4 space-y-4">
					{#each attention as entry (entry.entity_type + entry.source_file + entry.source_id + entry.source_name)}
						<li
							class="border-l-2 pl-4"
							style="border-color: var(--color-{RESULT_TONE[entry.result] ?? 'unknown'})"
						>
							<div class="flex flex-wrap items-baseline gap-2 text-sm">
								<span
									class="rounded px-1.5 py-0.5 text-[11px] font-medium"
									style="background-color: var(--color-{RESULT_TONE[entry.result] ??
										'unknown'}-soft)"
								>
									{t(`import.result.${entry.result}`)}
								</span>
								<span class="font-medium">{entry.source_name || entry.source_id}</span>
								<span class="muted text-xs">{t(`import.entity.${entry.entity_type}`)}</span>
							</div>
							{#if entry.detail}
								<p class="muted mt-1 text-sm">{entry.detail}</p>
							{/if}
						</li>
					{/each}
				</ul>
			{/if}
		</section>

		<section class="card overflow-x-auto p-5">
			<table class="w-full text-sm">
				<thead>
					<tr class="border-b text-left" style="border-color: var(--border)">
						<th class="muted py-2 pr-4 text-xs font-medium">{t('import.entity')}</th>
						{#each ['imported', 'renamed', 'skipped', 'failed', 'unsupported'] as column (column)}
							<th class="muted py-2 pr-4 text-xs font-medium">{t(`import.result.${column}`)}</th>
						{/each}
					</tr>
				</thead>
				<tbody>
					{#each Object.entries(job.summary) as [entity, counts] (entity)}
						<tr class="border-b last:border-0" style="border-color: var(--border)">
							<td class="py-2 pr-4">{t(`import.entity.${entity}`)}</td>
							{#each [counts.imported, counts.renamed, counts.skipped, counts.failed, counts.unsupported] as value, index (index)}
								<td class="py-2 pr-4 tabular-nums">{value}</td>
							{/each}
						</tr>
					{/each}
				</tbody>
			</table>
		</section>

		{#if job.state === 'partial'}
			<p class="muted text-sm">{t('import.partialHint')}</p>
		{/if}
		{#if !job.dry_run && !resume && job.state !== 'failed'}
			<p class="muted text-sm">{t('import.pausedHint')}</p>
		{/if}

		<div class="flex gap-2">
			<Button onclick={reset} variant="primary">{t('import.another')}</Button>
			{#if !job.dry_run}
				<Button href="/">{t('nav.monitors')}</Button>
			{/if}
		</div>
	</div>
{:else if running}
	<div class="card max-w-2xl p-8 text-center">
		<Spinner />
		<p class="mt-4 text-sm">{t('import.running')}</p>
		{#if job}
			<p class="muted mt-1 text-xs">{t(`import.state.${job.state}`)}</p>
		{/if}
	</div>
{:else}
	<form onsubmit={start} class="max-w-2xl space-y-6">
		<p class="muted text-sm">{t('import.intro')}</p>

		{#if error}
			<ErrorBox {error} />
		{/if}

		<section class="card space-y-5 p-5">
			<Field label={t('import.files')} id="files" hint={t('import.filesHint')}>
				{#snippet children({ id, describedBy })}
					<input
						{id}
						type="file"
						class="field"
						multiple
						accept=".db,.sqlite,.sqlite3,application/octet-stream"
						aria-describedby={describedBy}
						onchange={(event) => (files = event.currentTarget.files)}
					/>
				{/snippet}
			</Field>
			{#if files?.length}
				<ul class="muted space-y-1 text-xs">
					{#each Array.from(files) as file (file.name)}
						<li>{file.name} · {(file.size / 1_048_576).toFixed(1)} MB</li>
					{/each}
				</ul>
			{/if}
		</section>

		<section class="card space-y-5 p-5">
			<h2 class="font-semibold">{t('import.options')}</h2>

			<label class="flex items-start gap-2 text-sm">
				<input
					type="checkbox"
					class="mt-0.5 h-4 w-4 accent-[var(--accent)]"
					bind:checked={dryRun}
				/>
				<span>
					{t('import.dryRun')}
					<span class="muted block text-xs">{t('import.dryRunHint')}</span>
				</span>
			</label>

			<Field label={t('import.conflict')} id="conflict" hint={t('import.conflictHint')}>
				{#snippet children({ id, describedBy })}
					<select {id} class="field" bind:value={conflict} aria-describedby={describedBy}>
						{#each ['rename', 'skip', 'replace'] as value (value)}
							<option {value}>{t(`import.conflict.${value}`)}</option>
						{/each}
					</select>
				{/snippet}
			</Field>

			<Field label={t('import.namePrefix')} id="prefix" hint={t('import.namePrefixHint')} optional>
				{#snippet children({ id, describedBy })}
					<input
						{id}
						class="field"
						bind:value={namePrefix}
						placeholder="acme / "
						aria-describedby={describedBy}
					/>
				{/snippet}
			</Field>
		</section>

		<section class="card space-y-3 p-5">
			<h2 class="font-semibold">{t('import.what')}</h2>
			<label class="flex items-center gap-2 text-sm">
				<input
					type="checkbox"
					class="h-4 w-4 accent-[var(--accent)]"
					bind:checked={importMonitors}
				/>
				{t('import.monitors')}
			</label>
			<label class="flex items-center gap-2 text-sm">
				<input type="checkbox" class="h-4 w-4 accent-[var(--accent)]" bind:checked={importTags} />
				{t('import.tags')}
			</label>
			<label class="flex items-center gap-2 text-sm">
				<input
					type="checkbox"
					class="h-4 w-4 accent-[var(--accent)]"
					bind:checked={importNotifications}
				/>
				{t('import.notifications')}
			</label>
			<label class="flex items-center gap-2 text-sm">
				<input
					type="checkbox"
					class="h-4 w-4 accent-[var(--accent)]"
					bind:checked={importStatusPages}
				/>
				{t('import.statusPages')}
			</label>
			<label class="flex items-start gap-2 text-sm">
				<input
					type="checkbox"
					class="mt-0.5 h-4 w-4 accent-[var(--accent)]"
					bind:checked={importHistory}
				/>
				<span>
					{t('import.history')}
					<span class="muted block text-xs">{t('import.historyHint')}</span>
				</span>
			</label>
			<label class="flex items-start gap-2 text-sm">
				<input
					type="checkbox"
					class="mt-0.5 h-4 w-4 accent-[var(--accent)]"
					bind:checked={resume}
				/>
				<span>
					{t('import.resume')}
					<span class="muted block text-xs">{t('import.resumeHint')}</span>
				</span>
			</label>
		</section>

		<Button type="submit" variant="primary" disabled={running}>{t('import.start')}</Button>
	</form>
{/if}
