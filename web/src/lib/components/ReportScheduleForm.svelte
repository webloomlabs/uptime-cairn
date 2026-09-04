<script lang="ts">
	import { untrack } from 'svelte';
	import { api, ApiError } from '$lib/api';
	import type {
		NotificationChannel,
		Page as ApiPage,
		ReportFormat,
		ReportFrequency,
		ReportSchedule,
		ReportScheduleDelivery,
		ReportTemplate,
		Settings
	} from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import Field from '$lib/components/Field.svelte';
	import Button from '$lib/components/Button.svelte';
	import Icon from '$lib/components/Icon.svelte';

	/**
	 * The schedule editor: when a report fires, and who receives it.
	 *
	 * # This is the screen that makes reporting a product rather than an endpoint
	 *
	 * Everything else in this section describes a report. A schedule is the part
	 * that sends one to a client without anybody being at a keyboard on the first
	 * of the month, and until this existed the whole capability was reachable only
	 * by hand-writing JSON.
	 *
	 * # Two things the server refuses that this form has to respect
	 *
	 * **A schedule that will never fire is refused at save**, not discovered
	 * months later when a client asks where their report is — `0 0 30 2 *` is the
	 * 30th of February. So the cron box is validated by the server on every save
	 * and its error lands on the field.
	 *
	 * **`cron` is only accepted when the frequency is `cron`.** A stored
	 * expression that never runs is a schedule an operator believes they
	 * configured, so the server refuses the combination rather than ignoring the
	 * field — which means this form must clear it when the frequency changes,
	 * instead of leaving a value the save would reject.
	 *
	 * # Delivery targets are rows, not a fixed set of boxes
	 *
	 * One schedule can send the PDF to a client and the CSV to their data team,
	 * which is a real arrangement and not a hypothetical. Each row discloses only
	 * the fields its type uses.
	 */
	let {
		schedule = undefined,
		templateID = undefined,
		onsaved
	}: {
		schedule?: ReportSchedule;
		templateID?: string;
		onsaved: (saved: ReportSchedule) => void;
	} = $props();

	const editing = Boolean(schedule);

	let name = $state(schedule?.name ?? '');
	let reportTemplateID = $state(schedule?.report_template_id ?? templateID ?? '');
	let enabled = $state(schedule?.enabled ?? true);
	let frequency = $state<ReportFrequency>(schedule?.frequency ?? 'monthly');
	let cron = $state(schedule?.cron ?? '');
	let sendAt = $state(schedule?.send_at ?? '09:00');
	let timezone = $state(schedule?.timezone ?? '');

	/**
	 * The delivery rows, held as a local shape rather than as the wire type.
	 *
	 * Every field is a string here even where the API takes an array or a
	 * boolean, because a half-typed recipient list is a string and turning it
	 * into an array on every keystroke would fight the user. The conversion
	 * happens once, on save.
	 */
	type Row = {
		type: 'email' | 'slack' | 'webhook' | 's3';
		recipients: string;
		url: string;
		channelID: string;
		formats: ReportFormat[];
		bucket: string;
		prefix: string;
		region: string;
		endpoint: string;
		pathStyle: boolean;
		accessKeyID: string;
		secretAccessKey: string;
	};

	function blankRow(type: Row['type'] = 'email'): Row {
		return {
			type,
			recipients: '',
			url: '',
			channelID: '',
			formats: [],
			bucket: '',
			prefix: '',
			region: '',
			endpoint: '',
			pathStyle: false,
			accessKeyID: '',
			secretAccessKey: ''
		};
	}

	function rowFrom(delivery: ReportScheduleDelivery): Row {
		const row = blankRow(delivery.type);
		row.recipients = (delivery.recipients ?? []).join(', ');
		row.url = delivery.url ?? '';
		row.channelID = delivery.notification_channel_id ?? '';
		row.formats = delivery.formats ?? [];
		if (delivery.s3) {
			row.bucket = delivery.s3.bucket ?? '';
			row.prefix = delivery.s3.prefix ?? '';
			row.region = delivery.s3.region ?? '';
			row.endpoint = delivery.s3.endpoint ?? '';
			row.pathStyle = delivery.s3.path_style ?? false;
		}
		return row;
	}

	let rows = $state<Row[]>(
		schedule?.deliveries.length ? schedule.deliveries.map(rowFrom) : [blankRow()]
	);

	let templates = $state<ReportTemplate[]>([]);
	let channels = $state<NotificationChannel[]>([]);
	let instanceZone = $state('');

	let saving = $state(false);
	let failure = $state<string | null>(null);
	let fieldErrors = $state<Record<string, string>>({});

	const ALL_FORMATS: ReportFormat[] = ['pdf', 'html', 'csv', 'json'];
	const FREQUENCIES: ReportFrequency[] = ['daily', 'weekly', 'monthly', 'quarterly', 'cron'];
	const TYPES: Row['type'][] = ['email', 'slack', 'webhook', 's3'];

	$effect(() => {
		untrack(() => void loadReferences());
	});

	async function loadReferences() {
		const [templatePage, channelPage, settings] = await Promise.all([
			api
				.get<ApiPage<ReportTemplate>>('/report-templates?limit=200')
				.catch(() => ({ data: [] as ReportTemplate[] })),
			api
				.get<ApiPage<NotificationChannel>>('/notification-channels?limit=200')
				.catch(() => ({ data: [] as NotificationChannel[] })),
			api.get<Settings>('/settings').catch(() => null)
		]);
		templates = templatePage.data;
		channels = channelPage.data;
		instanceZone = settings?.general.timezone ?? '';
		// Only as a starting value, and only when creating. The stored zone is
		// what an existing schedule was cut in and must not drift towards the
		// instance's when somebody opens the form to change a recipient.
		if (!editing && !timezone) timezone = instanceZone;
	}

	/**
	 * The channels a delivery row may delegate to.
	 *
	 * Filtered by type, because a webhook target reading a Slack channel's URL
	 * would post a report description into a Slack incoming webhook and get a
	 * 400 nobody would connect to this screen.
	 */
	function channelsFor(type: Row['type']): NotificationChannel[] {
		return channels.filter((c) => c.type === type);
	}

	function toggleFormat(row: Row, format: ReportFormat) {
		row.formats = row.formats.includes(format)
			? row.formats.filter((f) => f !== format)
			: [...row.formats, format];
	}

	function addRow() {
		rows = [...rows, blankRow()];
	}

	function removeRow(at: number) {
		rows = rows.filter((_, i) => i !== at);
	}

	/**
	 * `weekly` and `quarterly` are not self-explanatory, and guessing wrong is a
	 * report that arrives on a day nobody expected. The server's own cron
	 * translation is the source of these sentences.
	 */
	function frequencyHint(f: ReportFrequency): string {
		return t(`schedules.frequencyHint.${f}`);
	}

	function deliveryBody(row: Row): Record<string, unknown> {
		const body: Record<string, unknown> = { type: row.type, formats: row.formats };
		if (row.channelID) body.notification_channel_id = row.channelID;

		switch (row.type) {
			case 'email':
				body.recipients = row.recipients
					.split(',')
					.map((address) => address.trim())
					.filter(Boolean);
				break;
			case 'slack':
			case 'webhook':
				if (row.url) body.url = row.url;
				break;
			case 's3':
				body.s3 = {
					bucket: row.bucket,
					prefix: row.prefix || null,
					region: row.region,
					endpoint: row.endpoint || null,
					path_style: row.pathStyle,
					access_key_id: row.accessKeyID,
					// Write-only and never read back, so an edit that leaves it
					// empty is refused by the server rather than silently storing
					// a blank credential. Said on the field rather than here.
					secret_access_key: row.secretAccessKey
				};
				break;
		}
		return body;
	}

	async function save(event: SubmitEvent) {
		event.preventDefault();
		saving = true;
		fieldErrors = {};
		failure = null;

		const body: Record<string, unknown> = {
			name,
			report_template_id: reportTemplateID,
			enabled,
			frequency,
			// **Cleared unless the frequency is cron.** The server refuses a
			// stored expression that will never run rather than ignoring it, so
			// sending the leftover value of a box the user has since hidden would
			// turn a frequency change into a validation error about a field they
			// cannot see.
			cron: frequency === 'cron' ? cron : null,
			timezone,
			send_at: sendAt,
			deliveries: rows.map(deliveryBody)
		};

		try {
			const saved = editing
				? await api.patch<ReportSchedule>(`/report-schedules/${schedule!.id}`, body)
				: await api.post<ReportSchedule>('/report-schedules', body);
			onsaved(saved);
		} catch (caught) {
			if (caught instanceof ApiError) {
				fieldErrors = caught.fields();
				failure = caught.message;
			} else {
				failure = t('common.retry');
			}
		} finally {
			saving = false;
		}
	}
</script>

<form onsubmit={save} class="max-w-2xl space-y-6">
	{#if failure}
		<div class="card p-4 text-sm" style="color: var(--color-down)">{failure}</div>
	{/if}

	<section class="card space-y-5 p-5">
		<Field label={t('schedules.name')} id="schedule-name" error={fieldErrors['/name']}>
			{#snippet children({ id, describedBy, invalid })}
				<input
					{id}
					class="field"
					bind:value={name}
					placeholder={t('schedules.namePlaceholder')}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				/>
			{/snippet}
		</Field>

		<Field
			label={t('schedules.template')}
			id="schedule-template"
			error={fieldErrors['/report_template_id']}
			hint={t('schedules.templateHint')}
		>
			{#snippet children({ id, describedBy, invalid })}
				<select
					{id}
					class="field"
					bind:value={reportTemplateID}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				>
					<option value="">{t('schedules.templateChoose')}</option>
					{#each templates as template (template.id)}
						<option value={template.id}>{template.name}</option>
					{/each}
				</select>
			{/snippet}
		</Field>

		<label class="flex items-center gap-2 text-sm">
			<input type="checkbox" class="h-4 w-4 accent-[var(--accent)]" bind:checked={enabled} />
			<span>{t('schedules.enabled')}</span>
		</label>
	</section>

	<section class="card space-y-5 p-5">
		<div>
			<h2 class="font-semibold">
				{t('schedules.when')}<span style="color: var(--color-up)" aria-hidden="true">.</span>
			</h2>
		</div>

		<Field
			label={t('schedules.frequency')}
			id="schedule-frequency"
			error={fieldErrors['/frequency']}
			hint={frequencyHint(frequency)}
		>
			{#snippet children({ id, describedBy, invalid })}
				<select
					{id}
					class="field"
					bind:value={frequency}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				>
					{#each FREQUENCIES as option (option)}
						<option value={option}>{t(`schedules.frequency.${option}`)}</option>
					{/each}
				</select>
			{/snippet}
		</Field>

		{#if frequency === 'cron'}
			<Field
				label={t('schedules.cron')}
				id="schedule-cron"
				error={fieldErrors['/cron']}
				hint={t('schedules.cronHint')}
			>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						class="field font-mono"
						placeholder="0 9 1 * *"
						bind:value={cron}
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>
		{:else}
			<!--
				Hidden for cron rather than disabled: the expression carries its own
				time, and a greyed-out box beside it invites the question of which
				one wins.
			-->
			<Field label={t('schedules.sendAt')} id="schedule-send-at" error={fieldErrors['/send_at']}>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						class="field sm:max-w-40"
						type="time"
						bind:value={sendAt}
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>
		{/if}

		<Field
			label={t('schedules.timezone')}
			id="schedule-timezone"
			error={fieldErrors['/timezone']}
			hint={instanceZone
				? t('schedules.timezoneHint', { zone: instanceZone })
				: t('schedules.timezoneHintNoDefault')}
		>
			{#snippet children({ id, describedBy, invalid })}
				<input
					{id}
					class="field"
					placeholder="Australia/Sydney"
					bind:value={timezone}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				/>
			{/snippet}
		</Field>
	</section>

	<section class="card space-y-5 p-5">
		<div>
			<h2 class="font-semibold">
				{t('schedules.deliveries')}<span style="color: var(--color-up)" aria-hidden="true">.</span>
			</h2>
			<p class="muted mt-1 text-sm">{t('schedules.deliveriesHint')}</p>
		</div>

		{#if fieldErrors['/deliveries']}
			<p class="text-sm" style="color: var(--color-down)">{fieldErrors['/deliveries']}</p>
		{/if}

		{#each rows as row, i (i)}
			<div class="space-y-4 rounded border p-4" style="border-color: var(--border)">
				<div class="flex items-start gap-3">
					<Field
						label={t('schedules.deliveryType')}
						id="delivery-type-{i}"
						error={fieldErrors[`/deliveries/${i}/type`]}
					>
						{#snippet children({ id, describedBy, invalid })}
							<select
								{id}
								class="field w-auto"
								bind:value={row.type}
								aria-describedby={describedBy}
								aria-invalid={invalid}
							>
								{#each TYPES as option (option)}
									<option value={option}>{t(`schedules.deliveryType.${option}`)}</option>
								{/each}
							</select>
						{/snippet}
					</Field>
					{#if rows.length > 1}
						<button
							type="button"
							class="mt-7 text-sm hover:underline"
							style="color: var(--color-down)"
							onclick={() => removeRow(i)}
						>
							{t('common.remove')}
						</button>
					{/if}
				</div>

				{#if row.type === 'email'}
					<Field
						label={t('schedules.recipients')}
						id="delivery-recipients-{i}"
						error={fieldErrors[`/deliveries/${i}/recipients`]}
						hint={t('schedules.recipientsHint')}
					>
						{#snippet children({ id, describedBy, invalid })}
							<input
								{id}
								class="field"
								placeholder="ops@acme.example, finance@acme.example"
								bind:value={row.recipients}
								aria-describedby={describedBy}
								aria-invalid={invalid}
							/>
						{/snippet}
					</Field>
				{/if}

				{#if row.type === 'slack' || row.type === 'webhook'}
					<Field
						label={t('schedules.url')}
						id="delivery-url-{i}"
						error={fieldErrors[`/deliveries/${i}/url`]}
						optional={channelsFor(row.type).length > 0}
					>
						{#snippet children({ id, describedBy, invalid })}
							<input
								{id}
								class="field"
								placeholder="https://hooks.example.com/…"
								bind:value={row.url}
								aria-describedby={describedBy}
								aria-invalid={invalid}
							/>
						{/snippet}
					</Field>
				{/if}

				<!--
					Delegating to a channel is offered wherever one of the right type
					exists, and it is the better answer: the target reads that
					channel's URL and secret headers at delivery rather than keeping a
					copy, so a rotated credential is rotated once. A copy would keep
					working until the old one was revoked and then fail in a way
					nobody would connect to the rotation.
				-->
				{#if channelsFor(row.type).length > 0}
					<Field
						label={t('schedules.channel')}
						id="delivery-channel-{i}"
						hint={t('schedules.channelHint')}
						optional
					>
						{#snippet children({ id, describedBy, invalid })}
							<select
								{id}
								class="field"
								bind:value={row.channelID}
								aria-describedby={describedBy}
								aria-invalid={invalid}
							>
								<option value="">{t('schedules.channelNone')}</option>
								{#each channelsFor(row.type) as channel (channel.id)}
									<option value={channel.id}>{channel.name}</option>
								{/each}
							</select>
						{/snippet}
					</Field>
				{/if}

				{#if row.type === 's3'}
					<!--
						The **drop**, and it is not the mirror. This puts one schedule's
						files in a bucket for a recipient; the mirror in Settings is a
						durability copy of every artifact. An operator who configures
						this believing they have durability has bought nothing, which is
						why the hint says so rather than leaving the two to be told
						apart by name.
					-->
					<p class="muted text-sm">{t('schedules.s3Hint')}</p>
					<div class="grid gap-4 sm:grid-cols-2">
						<Field
							label={t('settings.mirrorBucket')}
							id="delivery-bucket-{i}"
							error={fieldErrors[`/deliveries/${i}/s3/bucket`]}
						>
							{#snippet children({ id, describedBy, invalid })}
								<input
									{id}
									class="field"
									bind:value={row.bucket}
									aria-describedby={describedBy}
									aria-invalid={invalid}
								/>
							{/snippet}
						</Field>
						<Field label={t('settings.mirrorPrefix')} id="delivery-prefix-{i}" optional>
							{#snippet children({ id, describedBy, invalid })}
								<input
									{id}
									class="field"
									bind:value={row.prefix}
									aria-describedby={describedBy}
									aria-invalid={invalid}
								/>
							{/snippet}
						</Field>
						<Field
							label={t('settings.mirrorRegion')}
							id="delivery-region-{i}"
							error={fieldErrors[`/deliveries/${i}/s3/region`]}
							hint={t('settings.mirrorRegionHint')}
						>
							{#snippet children({ id, describedBy, invalid })}
								<input
									{id}
									class="field"
									placeholder="us-east-1"
									bind:value={row.region}
									aria-describedby={describedBy}
									aria-invalid={invalid}
								/>
							{/snippet}
						</Field>
						<Field
							label={t('settings.mirrorEndpoint')}
							id="delivery-endpoint-{i}"
							error={fieldErrors[`/deliveries/${i}/s3/endpoint`]}
							optional
						>
							{#snippet children({ id, describedBy, invalid })}
								<input
									{id}
									class="field"
									placeholder="https://minio.example.com:9000"
									bind:value={row.endpoint}
									aria-describedby={describedBy}
									aria-invalid={invalid}
								/>
							{/snippet}
						</Field>
						<Field
							label={t('settings.mirrorAccessKey')}
							id="delivery-access-{i}"
							error={fieldErrors[`/deliveries/${i}/s3/access_key_id`]}
						>
							{#snippet children({ id, describedBy, invalid })}
								<input
									{id}
									class="field"
									autocomplete="off"
									bind:value={row.accessKeyID}
									aria-describedby={describedBy}
									aria-invalid={invalid}
								/>
							{/snippet}
						</Field>
						<Field
							label={t('settings.mirrorSecretKey')}
							id="delivery-secret-{i}"
							error={fieldErrors[`/deliveries/${i}/s3/secret_access_key`]}
							hint={editing ? t('schedules.s3SecretOnEdit') : undefined}
						>
							{#snippet children({ id, describedBy, invalid })}
								<input
									{id}
									class="field"
									type="password"
									autocomplete="new-password"
									bind:value={row.secretAccessKey}
									aria-describedby={describedBy}
									aria-invalid={invalid}
								/>
							{/snippet}
						</Field>
					</div>
					<label class="flex items-center gap-2 text-sm">
						<input
							type="checkbox"
							class="h-4 w-4 accent-[var(--accent)]"
							bind:checked={row.pathStyle}
						/>
						<span>{t('settings.mirrorPathStyle')}</span>
					</label>
				{/if}

				<fieldset>
					<legend class="mb-1 block text-sm font-medium">{t('schedules.formats')}</legend>
					<p class="muted mb-2 text-sm">{t('schedules.formatsHint')}</p>
					<div class="flex flex-wrap gap-3">
						{#each ALL_FORMATS as format (format)}
							<label class="flex items-center gap-1.5 text-sm">
								<input
									type="checkbox"
									checked={row.formats.includes(format)}
									onchange={() => toggleFormat(row, format)}
								/>
								{format.toUpperCase()}
							</label>
						{/each}
					</div>
					{#if fieldErrors[`/deliveries/${i}/formats`]}
						<p class="mt-1 text-sm" style="color: var(--color-down)">
							{fieldErrors[`/deliveries/${i}/formats`]}
						</p>
					{/if}
				</fieldset>
			</div>
		{/each}

		<button
			type="button"
			class="inline-flex items-center gap-1 text-sm hover:underline"
			onclick={addRow}
		>
			<Icon name="plus" size={15} />
			{t('schedules.addDelivery')}
		</button>
	</section>

	<Button type="submit" variant="primary" loading={saving} disabled={saving}>
		{editing ? t('common.save') : t('schedules.create')}
	</Button>
</form>
