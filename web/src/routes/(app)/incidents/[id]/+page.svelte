<script lang="ts">
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, ApiError } from '$lib/api';
	import type { Incident, IncidentUpdate } from '$lib/types';
	import { INCIDENT_STATES } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import { formatAbsolute, formatDuration, formatRelative } from '$lib/format';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Button from '$lib/components/Button.svelte';
	import Field from '$lib/components/Field.svelte';
	import Icon from '$lib/components/Icon.svelte';

	/**
	 * One incident, and the only place its state can be advanced.
	 *
	 * There is no state dropdown on the edit screen, and that is the server's
	 * design rather than a shortcut here: an incident advances through its
	 * timeline, so every state change carries the sentence explaining it. The
	 * form below is therefore not "post a comment" — it is the mechanism, and
	 * moving to `resolved` without saying what fixed it is not something the API
	 * offers.
	 */
	let incident = $state<Incident | null>(null);
	let error = $state<unknown>(null);
	let loading = $state(true);

	let body = $state('');
	let nextState = $state('');
	let notify = $state(false);
	let posting = $state(false);
	let postError = $state<unknown>(null);
	let fields = $state<Record<string, string>>({});

	async function load() {
		loading = true;
		error = null;
		try {
			incident = await api.get<Incident>(`/incidents/${page.params.id}`);
		} catch (caught) {
			error = caught;
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		untrack(() => void load());
	});

	async function post(event: SubmitEvent) {
		event.preventDefault();
		if (!incident) return;
		posting = true;
		postError = null;
		fields = {};
		try {
			await api.post<IncidentUpdate>(`/incidents/${incident.id}/updates`, {
				state: nextState || undefined,
				body,
				notify_subscribers: notify
			});
			body = '';
			nextState = '';
			notify = false;
			await load();
		} catch (caught) {
			postError = caught;
			if (caught instanceof ApiError) fields = caught.fields();
		} finally {
			posting = false;
		}
	}

	async function remove() {
		if (!incident) return;
		if (!confirm(t('incidents.deleteConfirm'))) return;
		try {
			await api.delete(`/incidents/${incident.id}`);
			await goto('/incidents');
		} catch (caught) {
			error = caught;
		}
	}

	const open = $derived(incident !== null && incident.resolved_at === null);

	const elapsed = $derived.by(() => {
		if (!incident) return '';
		const end = incident.resolved_at ? Date.parse(incident.resolved_at) : Date.now();
		return formatDuration(Math.max(0, Math.round((end - Date.parse(incident.started_at)) / 1000)));
	});

	const IMPACT_TONE: Record<string, string> = {
		critical: 'down',
		major: 'down',
		minor: 'pending',
		none: 'paused'
	};

	/** The timeline, newest first, which is the order somebody catching up reads. */
	const timeline = $derived([...(incident?.updates ?? [])].reverse());
</script>

<PageTitle title={incident?.title ?? t('nav.incidents')}>
	{#snippet actions()}
		{#if incident && session.allows('incidents:write')}
			<Button href="/incidents/{incident.id}/edit">{t('common.edit')}</Button>
			<Button variant="danger" onclick={remove}>{t('common.delete')}</Button>
		{/if}
	{/snippet}
</PageTitle>

{#if loading}
	<Spinner />
{:else if error}
	<ErrorBox {error} onretry={load} />
{:else if incident}
	<div class="flex flex-col gap-6 xl:flex-row">
		<div class="min-w-0 flex-1 space-y-6">
			<section class="card p-5">
				<div class="flex flex-wrap items-center gap-3">
					<span
						class="inline-flex items-center gap-1.5 font-medium"
						style="color: var(--color-{open ? 'down' : 'up'})"
					>
						<Icon name={open ? 'incidents' : 'check'} size={16} />
						{t(`incidents.state.${incident.state}`)}
					</span>
					<span
						class="rounded px-2 py-0.5 text-xs font-medium"
						style="background-color: var(--color-{IMPACT_TONE[incident.impact] ?? 'unknown'}-soft)"
					>
						{t(`incidents.impact.${incident.impact}`)}
					</span>
					{#if incident.auto_opened}
						<span class="muted text-xs">{t('incidents.autoOpenedHint')}</span>
					{/if}
				</div>

				<dl class="mt-4 grid gap-4 text-sm sm:grid-cols-3">
					<div>
						<dt class="muted text-xs">{t('incidents.started')}</dt>
						<dd>{formatAbsolute(incident.started_at)}</dd>
					</div>
					<div>
						<dt class="muted text-xs">{t('incidents.resolved')}</dt>
						<dd>{incident.resolved_at ? formatAbsolute(incident.resolved_at) : '—'}</dd>
					</div>
					<div>
						<dt class="muted text-xs">{t('incidents.duration')}</dt>
						<dd class="tabular-nums">{elapsed}</dd>
					</div>
				</dl>
			</section>

			{#if session.allows('incidents:write')}
				<section class="card space-y-5 p-5">
					<div>
						<h2 class="font-semibold">{t('incidents.postUpdate')}</h2>
						<p class="muted mt-1 text-sm">{t('incidents.stateChangeNote')}</p>
					</div>

					{#if postError}
						<ErrorBox error={postError} />
					{/if}

					<form onsubmit={post} class="space-y-4">
						<Field label={t('incidents.updateBody')} id="update-body" error={fields['/body']}>
							{#snippet children({ id, describedBy, invalid })}
								<textarea
									{id}
									class="field min-h-24"
									bind:value={body}
									required
									aria-describedby={describedBy}
									aria-invalid={invalid}
								></textarea>
							{/snippet}
						</Field>

						<Field label={t('incidents.advanceTo')} id="update-state" error={fields['/state']}>
							{#snippet children({ id, describedBy, invalid })}
								<select
									{id}
									class="field"
									bind:value={nextState}
									aria-describedby={describedBy}
									aria-invalid={invalid}
								>
									<option value="">{t('incidents.keepState')}</option>
									{#each INCIDENT_STATES as value (value)}
										<option {value}>{t(`incidents.state.${value}`)}</option>
									{/each}
								</select>
							{/snippet}
						</Field>

						{#if incident.status_page_ids.length}
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

						<Button type="submit" variant="primary" loading={posting} disabled={posting}>
							{t('incidents.postUpdate')}
						</Button>
					</form>
				</section>
			{/if}

			<section class="card p-5">
				<h2 class="font-semibold">
					{t('incidents.timeline')}<span style="color: var(--color-up)" aria-hidden="true">.</span>
				</h2>

				{#if timeline.length === 0}
					<p class="muted mt-4 text-sm">{t('incidents.noUpdates')}</p>
				{:else}
					<ol class="mt-5 space-y-5">
						{#each timeline as entry (entry.id)}
							<li class="border-l-2 pl-4" style="border-color: var(--border)">
								<div class="flex flex-wrap items-center gap-2 text-sm">
									{#if entry.state}
										<span class="font-medium">{t(`incidents.state.${entry.state}`)}</span>
									{/if}
									<span class="muted text-xs" title={formatAbsolute(entry.created_at)}>
										{formatRelative(entry.created_at)}
									</span>
									<span class="muted text-xs">
										·
										{entry.notified_subscribers
											? t('incidents.notified')
											: t('incidents.notNotified')}
									</span>
								</div>
								<p class="mt-1 text-sm whitespace-pre-wrap">{entry.body}</p>
							</li>
						{/each}
					</ol>
				{/if}
			</section>
		</div>

		{#if incident.metrics}
			<aside class="w-full shrink-0 xl:w-64">
				<section class="card p-5">
					<h2 class="font-semibold">
						{t('incidents.metrics')}<span style="color: var(--color-up)" aria-hidden="true">.</span>
					</h2>
					<dl class="mt-4 space-y-3 text-sm">
						{#each [{ k: 'timeToDetect', v: incident.metrics.time_to_detect_seconds }, { k: 'timeToAcknowledge', v: incident.metrics.time_to_acknowledge_seconds }, { k: 'timeToResolve', v: incident.metrics.time_to_resolve_seconds }] as row (row.k)}
							<div class="flex items-baseline justify-between gap-2">
								<dt class="muted text-xs">{t(`incidents.${row.k}`)}</dt>
								<!-- Null is "has not happened yet", which is a different
								     claim from zero and is drawn as one. -->
								<dd class="tabular-nums">{row.v === null ? '—' : formatDuration(row.v)}</dd>
							</div>
						{/each}
					</dl>
				</section>
			</aside>
		{/if}
	</div>
{/if}
