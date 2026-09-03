<script lang="ts">
	import { untrack } from 'svelte';
	import { api, ApiError, getCSRFToken } from '$lib/api';
	import type { BrandProfile, Page as ApiPage } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Button from '$lib/components/Button.svelte';
	import Field from '$lib/components/Field.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import { session } from '$lib/session.svelte';

	/**
	 * Brand profiles.
	 *
	 * The screen exists for one job — giving a client's report their name, their
	 * colour and their logo — so it is one list with an inline editor rather than
	 * three routes. There are rarely more than a handful of these.
	 *
	 * The part worth care is the **logo refusal**. SVG is the format somebody
	 * will reach for: it is what a brand pack contains and what the project's own
	 * mark is. It cannot be embedded in a PDF by this renderer, so it is refused
	 * at upload — and the refusal has to explain itself *here*, where somebody is
	 * holding the SVG, rather than as a server error code.
	 */
	let profiles = $state<BrandProfile[]>([]);
	let loading = $state(true);
	let error = $state<unknown>(null);
	let editing = $state<string | null>(null);
	let creating = $state(false);
	let saving = $state(false);
	let fieldErrors = $state<Record<string, string>>({});
	let logoProblem = $state<Record<string, string>>({});

	let draft = $state({
		name: '',
		company_name: '',
		primary_color: '',
		accent_color: '',
		cover_text: '',
		footer_text: '',
		hide_powered_by: false,
		is_default: false
	});

	async function load() {
		loading = true;
		error = null;
		try {
			profiles = (await api.get<ApiPage<BrandProfile>>('/brand-profiles?limit=100')).data;
		} catch (caught) {
			error = caught;
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		untrack(() => void load());
	});

	function startCreate() {
		draft = {
			name: '',
			company_name: '',
			primary_color: '',
			accent_color: '',
			cover_text: '',
			footer_text: '',
			hide_powered_by: false,
			is_default: profiles.length === 0
		};
		fieldErrors = {};
		editing = null;
		creating = true;
	}

	function startEdit(profile: BrandProfile) {
		draft = {
			name: profile.name,
			company_name: profile.company_name ?? '',
			primary_color: profile.primary_color ?? '',
			accent_color: profile.accent_color ?? '',
			cover_text: profile.cover_text ?? '',
			footer_text: profile.footer_text ?? '',
			hide_powered_by: profile.hide_powered_by,
			is_default: profile.is_default
		};
		fieldErrors = {};
		creating = false;
		editing = profile.id;
	}

	async function save(event: SubmitEvent) {
		event.preventDefault();
		saving = true;
		fieldErrors = {};
		try {
			if (creating) {
				await api.post<BrandProfile>('/brand-profiles', draft);
			} else {
				await api.patch<BrandProfile>(`/brand-profiles/${editing}`, draft);
			}
			creating = false;
			editing = null;
			await load();
		} catch (caught) {
			if (caught instanceof ApiError) fieldErrors = caught.fields();
		} finally {
			saving = false;
		}
	}

	/**
	 * Upload the bytes as they are.
	 *
	 * The server decides the format **from the bytes rather than from the
	 * declared type**, which is what makes the SVG refusal reliable: browsers and
	 * command-line tools both mislabel often enough that a header check would
	 * fail in exactly the case this exists for. So the browser's own guess is
	 * sent and the server's verdict is the one that counts.
	 */
	async function uploadLogo(profile: BrandProfile, file: File) {
		delete logoProblem[profile.id];
		try {
			const response = await fetch(`/api/v1/brand-profiles/${profile.id}/logo`, {
				method: 'PUT',
				credentials: 'same-origin',
				headers: {
					'Content-Type': file.type || 'application/octet-stream',
					...(getCSRFToken() ? { 'X-Cairn-CSRF-Token': getCSRFToken()! } : {})
				},
				body: file
			});
			if (!response.ok) {
				const problem = await response.json().catch(() => null);
				// A 415 is the SVG case almost every time, and the server's own
				// sentence already names the fix. It is shown verbatim rather than
				// replaced, so the two cannot drift.
				logoProblem[profile.id] = problem?.detail ?? t('brands.logoSvgRefused');
				return;
			}
			await load();
		} catch {
			logoProblem[profile.id] = t('common.retry');
		}
	}

	async function remove(profile: BrandProfile) {
		try {
			await api.delete(`/brand-profiles/${profile.id}`);
			await load();
		} catch (caught) {
			// A 409 counts the templates still using it. That count is the useful
			// part, so the server's sentence is shown rather than a generic
			// refusal.
			if (caught instanceof ApiError) logoProblem[profile.id] = caught.message;
		}
	}

	const canWrite = $derived(session.allows('brand_profiles:write'));
</script>

<PageTitle title={t('reports.brands')}>
	{#snippet actions()}
		{#if canWrite && !creating}
			<Button variant="primary" onclick={startCreate}>
				<Icon name="plus" size={16} />
				{t('brands.new')}
			</Button>
		{/if}
	{/snippet}
</PageTitle>

{#if loading}
	<Spinner />
{:else if error}
	<ErrorBox {error} onretry={load} />
{:else}
	{#if creating || editing}
		<form class="card mb-5 space-y-5 p-5" onsubmit={save}>
			<Field label={t('reports.name')} id="brand-name" error={fieldErrors['/name']}>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						class="field"
						bind:value={draft.name}
						required
						maxlength="200"
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>

			<Field
				label={t('brands.companyName')}
				id="brand-company"
				optional
				hint={t('brands.companyNameHint')}
				error={fieldErrors['/company_name']}
			>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						class="field"
						bind:value={draft.company_name}
						maxlength="200"
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>

			<div class="grid gap-5 sm:grid-cols-2">
				<Field
					label={t('brands.primaryColor')}
					id="brand-primary"
					optional
					hint={t('brands.colorHint')}
					error={fieldErrors['/primary_color']}
				>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							class="field"
							placeholder="#0B5FFF"
							bind:value={draft.primary_color}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
				<Field
					label={t('brands.accentColor')}
					id="brand-accent"
					optional
					error={fieldErrors['/accent_color']}
				>
					{#snippet children({ id, describedBy, invalid })}
						<input
							{id}
							class="field"
							placeholder="#1A8F5A"
							bind:value={draft.accent_color}
							aria-describedby={describedBy}
							aria-invalid={invalid}
						/>
					{/snippet}
				</Field>
			</div>

			<Field
				label={t('brands.coverText')}
				id="brand-cover"
				optional
				hint={t('brands.textHint')}
				error={fieldErrors['/cover_text']}
			>
				{#snippet children({ id, describedBy, invalid })}
					<textarea
						{id}
						class="field"
						rows="2"
						bind:value={draft.cover_text}
						aria-describedby={describedBy}
						aria-invalid={invalid}
					></textarea>
				{/snippet}
			</Field>

			<Field
				label={t('brands.footerText')}
				id="brand-footer"
				optional
				error={fieldErrors['/footer_text']}
			>
				{#snippet children({ id, describedBy, invalid })}
					<input
						{id}
						class="field"
						bind:value={draft.footer_text}
						aria-describedby={describedBy}
						aria-invalid={invalid}
					/>
				{/snippet}
			</Field>

			<label class="flex items-center gap-2 text-sm">
				<input type="checkbox" bind:checked={draft.hide_powered_by} />
				{t('brands.hidePoweredBy')}
			</label>

			<label class="flex items-start gap-2 text-sm">
				<input type="checkbox" class="mt-0.5" bind:checked={draft.is_default} />
				<span>
					{t('brands.isDefault')}
					<span class="muted block text-xs">{t('brands.isDefaultHint')}</span>
				</span>
			</label>

			<div class="flex gap-2">
				<Button type="submit" variant="primary" loading={saving} disabled={saving}>
					{creating ? t('common.create') : t('common.save')}
				</Button>
				<Button
					onclick={() => {
						creating = false;
						editing = null;
					}}>{t('common.cancel')}</Button
				>
			</div>
		</form>
	{/if}

	{#if profiles.length === 0 && !creating}
		<div class="card px-4 py-14 text-center">
			<p class="font-medium">{t('brands.empty')}</p>
			<!--
				Says what happens *without* a profile, which is the useful thing:
				reports are still branded, with the instance's own name and colour.
				An empty state that only says "none yet" reads as broken.
			-->
			<p class="muted mx-auto mt-1 max-w-md text-sm">{t('brands.emptyHint')}</p>
			{#if canWrite}
				<div class="mt-5">
					<Button variant="primary" onclick={startCreate}>{t('brands.new')}</Button>
				</div>
			{/if}
		</div>
	{:else}
		<ul class="space-y-3">
			{#each profiles as profile (profile.id)}
				<li class="card p-4">
					<div class="flex flex-wrap items-start justify-between gap-3">
						<div class="min-w-0">
							<p class="flex items-center gap-2 font-medium">
								{profile.company_name || profile.name}
								{#if profile.is_default}
									<span
										class="rounded px-1.5 py-0.5 text-xs font-normal"
										style="background-color: var(--surface-hover)">{t('common.default')}</span
									>
								{/if}
							</p>
							<p class="muted mt-0.5 text-xs">
								{profile.name}
								{#if profile.primary_color}
									<span class="ml-2 inline-flex items-center gap-1">
										<span
											class="inline-block h-3 w-3 rounded-sm align-middle"
											style="background-color: {profile.primary_color}"
											aria-hidden="true"
										></span>
										{profile.primary_color}
									</span>
								{/if}
								{#if profile.logo_content_type}
									<span class="ml-2">· {t('brands.logoSet')}</span>
								{/if}
							</p>
						</div>

						{#if canWrite}
							<div class="flex shrink-0 flex-wrap items-center gap-2">
								<label class="cursor-pointer text-sm hover:underline">
									{t('brands.logoUpload')}
									<input
										type="file"
										class="sr-only"
										accept="image/png,image/jpeg"
										onchange={(event) => {
											const input = event.currentTarget as HTMLInputElement;
											const file = input.files?.[0];
											if (file) void uploadLogo(profile, file);
											input.value = '';
										}}
									/>
								</label>
								<Button size="sm" onclick={() => startEdit(profile)}>{t('common.edit')}</Button>
								<Button size="sm" variant="danger" onclick={() => remove(profile)}>
									{t('common.delete')}
								</Button>
							</div>
						{/if}
					</div>

					<p class="muted mt-2 text-xs">{t('brands.logoHint')}</p>

					{#if logoProblem[profile.id]}
						<p class="mt-2 text-sm" style="color: var(--color-down)">{logoProblem[profile.id]}</p>
					{/if}
				</li>
			{/each}
		</ul>
	{/if}
{/if}
