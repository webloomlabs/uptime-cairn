<script lang="ts">
	import { goto } from '$app/navigation';
	import { api, ApiError } from '$lib/api';
	import type { Incident, Page as ApiPage, StatusPage } from '$lib/types';
	import { INCIDENT_IMPACTS, INCIDENT_STATES } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import Field from './Field.svelte';
	import Button from './Button.svelte';
	import ErrorBox from './ErrorBox.svelte';
	import MonitorPicker from './MonitorPicker.svelte';

	/**
	 * Opening an incident, and editing the parts of one that are metadata.
	 *
	 * `state` is on this form only when opening. An existing incident's state is
	 * deliberately not editable here, and that is the server's rule rather than
	 * a simplification: an incident advances through its timeline, so that every
	 * state change carries the sentence explaining it. A dropdown that moved an
	 * incident from investigating to identified with nobody saying what was
	 * identified is precisely the record a post-mortem cannot reconstruct.
	 *
	 * Validation is entirely the server's, like every other form here: failures
	 * come back as RFC 9457 with a JSON pointer per bad field, and those pointers
	 * are what highlight the controls. The form has no opinion of its own to
	 * disagree with.
	 */
	let { incident = undefined }: { incident?: Incident } = $props();

	const editing = incident !== undefined;

	// Seeded once from the prop, then owned by the form. Svelte warns that these
	// capture only the initial value, and that is what is wanted: a route that
	// re-fetched the incident while somebody was halfway through editing it and
	// silently replaced their unsaved text would be a worse bug than the one the
	// warning is pointing at. The route remounts the form when the id changes,
	// which is where a genuinely different incident comes from.

	let title = $state(incident?.title ?? '');
	let impact = $state(incident?.impact ?? 'minor');
	// Named initialState, not state: a local called `state` shadows the $state
	// rune for the rest of the block, and every declaration after it fails to
	// compile with an error that points at the rune rather than at the shadow.
	let initialState = $state<string>('investigating');
	let body = $state('');
	let startedAt = $state(
		incident ? toLocalInput(incident.started_at) : toLocalInput(new Date().toISOString())
	);
	let monitorIds = $state<string[]>(incident?.monitor_ids ?? []);
	/**
	 * The chosen monitors with their names, for the chips.
	 *
	 * Fetched one at a time on edit, which is a fan-out and is fine here for a
	 * reason that does not apply to the monitor list: an incident names the
	 * handful of services that are actually broken, not a page of an install.
	 * The list endpoint has no "these ids" filter, and adding one to serve a
	 * control that asks for four rows would be API surface earning nothing.
	 */
	let picked = $state<{ id: string; name: string }[]>([]);
	const chosen = $derived(new Set(picked.map((m) => m.id)));

	function remove(id: string) {
		picked = picked.filter((m) => m.id !== id);
		monitorIds = picked.map((m) => m.id);
	}
	let pageIds = $state<string[]>(incident?.status_page_ids ?? []);
	let notify = $state(false);

	let pages = $state<StatusPage[]>([]);
	let saving = $state(false);
	let error = $state<unknown>(null);
	let fields = $state<Record<string, string>>({});

	/**
	 * `datetime-local` wants a value with no zone and renders it as local time.
	 * The API speaks RFC 3339 in UTC. Converting through the epoch rather than
	 * through string surgery is what makes an incident backdated across a
	 * daylight-saving boundary land on the hour the operator meant.
	 */
	function toLocalInput(iso: string): string {
		const at = new Date(iso);
		const offset = at.getTimezoneOffset() * 60_000;
		return new Date(at.getTime() - offset).toISOString().slice(0, 16);
	}

	function fromLocalInput(value: string): string | undefined {
		if (!value) return undefined;
		const at = new Date(value);
		return Number.isNaN(at.getTime()) ? undefined : at.toISOString();
	}

	$effect(() => {
		(async () => {
			for (const id of incident?.monitor_ids ?? []) {
				try {
					const monitor = await api.get<{ id: string; name: string }>(`/monitors/${id}`);
					picked = [...picked, { id: monitor.id, name: monitor.name }];
				} catch {
					// A monitor deleted since the incident was opened. Its id stays
					// on the incident — the record is of what was affected at the
					// time — and it simply has no chip.
				}
			}
		})();

		(async () => {
			if (!session.allows('status_pages:read')) return;
			try {
				pages = (await api.get<ApiPage<StatusPage>>('/status-pages?limit=200')).data;
			} catch {
				// The control hides itself rather than breaking the form. An
				// incident with no page attached is perfectly valid — it is an
				// internal record until somebody publishes it.
				pages = [];
			}
		})();
	});

	function toggle(list: string[], id: string): string[] {
		return list.includes(id) ? list.filter((v) => v !== id) : [...list, id];
	}

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		saving = true;
		error = null;
		fields = {};

		try {
			if (editing && incident) {
				await api.patch<Incident>(`/incidents/${incident.id}`, {
					title,
					impact,
					started_at: fromLocalInput(startedAt),
					monitor_ids: monitorIds,
					status_page_ids: pageIds
				});
				await goto(`/incidents/${incident.id}`);
				return;
			}

			const created = await api.post<Incident>('/incidents', {
				title,
				state: initialState,
				impact,
				started_at: fromLocalInput(startedAt),
				monitor_ids: monitorIds,
				status_page_ids: pageIds,
				body,
				notify_subscribers: notify
			});
			await goto(`/incidents/${created.id}`);
		} catch (caught) {
			error = caught;
			if (caught instanceof ApiError) fields = caught.fields();
		} finally {
			saving = false;
		}
	}
</script>

<form onsubmit={submit} class="max-w-2xl space-y-6">
	{#if error}
		<ErrorBox {error} />
	{/if}

	<section class="card space-y-5 p-5">
		<Field
			label={t('incidents.titleLabel')}
			id="title"
			error={fields['/title']}
			hint={t('incidents.titleHint')}
		>
			{#snippet children({ id, describedBy, invalid })}
				<input
					{id}
					class="field"
					bind:value={title}
					required
					maxlength="200"
					aria-describedby={describedBy}
					aria-invalid={invalid}
				/>
			{/snippet}
		</Field>

		<Field
			label={t('incidents.impact')}
			id="impact"
			error={fields['/impact']}
			hint={t('incidents.impactHint')}
		>
			{#snippet children({ id, describedBy, invalid })}
				<select
					{id}
					class="field"
					bind:value={impact}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				>
					{#each INCIDENT_IMPACTS as value (value)}
						<option {value}>{t(`incidents.impact.${value}`)}</option>
					{/each}
				</select>
			{/snippet}
		</Field>

		{#if !editing}
			<Field label={t('incidents.initialState')} id="state" error={fields['/state']}>
				{#snippet children({ id, describedBy, invalid })}
					<select
						{id}
						class="field"
						bind:value={initialState}
						aria-describedby={describedBy}
						aria-invalid={invalid}
					>
						{#each INCIDENT_STATES as value (value)}
							<option {value}>{t(`incidents.state.${value}`)}</option>
						{/each}
					</select>
				{/snippet}
			</Field>
		{/if}

		<Field
			label={t('incidents.startedAt')}
			id="started"
			error={fields['/started_at']}
			hint={t('incidents.startedAtHint')}
		>
			{#snippet children({ id, describedBy, invalid })}
				<input
					{id}
					type="datetime-local"
					class="field"
					bind:value={startedAt}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				/>
			{/snippet}
		</Field>

		{#if !editing}
			<Field
				label={t('incidents.firstUpdate')}
				id="body"
				error={fields['/body']}
				hint={t('incidents.firstUpdateHint')}
			>
				{#snippet children({ id, describedBy, invalid })}
					<textarea
						{id}
						class="field min-h-28"
						bind:value={body}
						required
						aria-describedby={describedBy}
						aria-invalid={invalid}
					></textarea>
				{/snippet}
			</Field>
		{/if}
	</section>

	<section class="card space-y-5 p-5">
		<div>
			<h2 class="font-medium">{t('incidents.affected')}</h2>
			<p class="muted mt-1 text-sm">{t('incidents.affectedHint')}</p>
		</div>
		{#if picked.length}
			<ul class="flex flex-wrap gap-2">
				{#each picked as monitor (monitor.id)}
					<li
						class="flex items-center gap-1.5 rounded-md px-2 py-1 text-sm"
						style="background-color: var(--surface-sunken)"
					>
						<span>{monitor.name}</span>
						<button
							type="button"
							class="muted hover:text-[var(--text)]"
							aria-label={`${t('common.remove')} ${monitor.name}`}
							onclick={() => remove(monitor.id)}
						>
							&times;
						</button>
					</li>
				{/each}
			</ul>
		{/if}

		<!-- A server-side search rather than a <select> of every monitor: a
		     control that ships the whole collection to the browser is the exact
		     mechanism ADR-004 exists to prevent. -->
		<MonitorPicker
			id="affected"
			exclude={chosen}
			onpick={(monitor) => {
				picked = [...picked, monitor];
				monitorIds = picked.map((m) => m.id);
			}}
		/>
	</section>

	{#if pages.length}
		<section class="card space-y-4 p-5">
			<div>
				<h2 class="font-medium">{t('incidents.onPages')}</h2>
				<p class="muted mt-1 text-sm">{t('incidents.onPagesHint')}</p>
			</div>
			<div class="space-y-2">
				{#each pages as page (page.id)}
					<label class="flex items-center gap-2 text-sm">
						<input
							type="checkbox"
							class="h-4 w-4 accent-[var(--accent)]"
							checked={pageIds.includes(page.id)}
							onchange={() => (pageIds = toggle(pageIds, page.id))}
						/>
						<span>{page.title}</span>
						<span class="muted text-xs">/status/{page.slug}</span>
					</label>
				{/each}
			</div>

			{#if !editing}
				<label class="flex items-start gap-2 text-sm">
					<input
						type="checkbox"
						class="mt-0.5 h-4 w-4 accent-[var(--accent)]"
						bind:checked={notify}
					/>
					<span>
						{t('incidents.notifySubscribers')}
						<span class="muted block text-xs">{t('incidents.notifyHint')}</span>
					</span>
				</label>
				{#if notify && session.info?.capabilities?.subscriber_delivery === false}
					<p class="text-sm" style="color: var(--color-pending)">
						{t('incidents.subscriberDeliveryOff')}
					</p>
				{/if}
			{/if}
		</section>
	{/if}

	<div class="flex gap-2">
		<Button type="submit" variant="primary" loading={saving} disabled={saving}>
			{editing ? t('common.save') : t('incidents.openIncident')}
		</Button>
		<Button href={editing && incident ? `/incidents/${incident.id}` : '/incidents'}>
			{t('common.cancel')}
		</Button>
	</div>
</form>
