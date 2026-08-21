<script lang="ts">
	import { untrack } from 'svelte';
	import { api, ApiError } from '$lib/api';
	import type { Tag } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import Button from './Button.svelte';
	import Field from './Field.svelte';
	import ErrorBox from './ErrorBox.svelte';

	/**
	 * Create or edit a tag.
	 *
	 * The slug is deliberately not previewed while typing. It is derived from the
	 * name by the server so that two tags cannot render identically in a list,
	 * and reproducing that derivation here would be a second implementation of
	 * one rule — the kind that agrees in testing and disagrees on the first name
	 * with an accent in it. An existing tag shows the slug the server actually
	 * assigned; a new one gets a sentence saying where it comes from.
	 */
	let {
		tag = null,
		onsaved,
		oncancel
	}: {
		tag?: Tag | null;
		onsaved: () => void;
		oncancel: () => void;
	} = $props();

	const editing = $derived(Boolean(tag));

	/** internal/model.DefaultTagColor — a neutral grey, matching the spec's default. */
	const DEFAULT_COLOR = '#6b7280';

	// Captured once; the caller remounts with {#key} when it switches record.
	const seed = untrack(() => ({
		name: tag?.name ?? '',
		description: tag?.description ?? '',
		color: tag?.color ?? DEFAULT_COLOR
	}));

	let name = $state(seed.name);
	let description = $state(seed.description);
	let color = $state(seed.color);

	let saving = $state(false);
	let error = $state<unknown>(null);
	let fieldErrors = $state<Record<string, string>>({});

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		saving = true;
		error = null;
		fieldErrors = {};

		const body = {
			name,
			color,
			description: description.trim() === '' ? null : description
		};

		try {
			if (editing) await api.patch(`/tags/${tag!.id}`, body);
			else await api.post('/tags', body);
			onsaved();
		} catch (caught) {
			if (caught instanceof ApiError && caught.problem.errors?.length) {
				fieldErrors = caught.fields();
				error = new ApiError({ ...caught.problem, detail: t('form.fixFields') });
			} else {
				error = caught;
			}
		} finally {
			saving = false;
		}
	}
</script>

<form class="space-y-4" onsubmit={submit}>
	{#if error}
		<ErrorBox {error} />
	{/if}

	<Field
		label={t('form.name')}
		id="tag-name"
		hint={editing ? t('taxonomy.slugIs', { slug: tag!.slug }) : t('taxonomy.slugHint')}
		error={fieldErrors['/name']}
	>
		{#snippet children({ id, describedBy, invalid })}
			<input
				{id}
				class="field"
				bind:value={name}
				maxlength="100"
				required
				aria-describedby={describedBy}
				aria-invalid={invalid}
			/>
		{/snippet}
	</Field>

	<Field label={t('taxonomy.colour')} id="tag-color" error={fieldErrors['/color']}>
		{#snippet children({ id, describedBy, invalid })}
			<div class="flex items-center gap-3">
				<!-- A colour input rather than a hex field: the server takes a
				     six-digit triple and nothing else, and a picker cannot produce
				     anything it would refuse. -->
				<input
					{id}
					type="color"
					class="h-9 w-14 cursor-pointer rounded-lg border p-1"
					style="border-color: var(--border); background-color: var(--surface-raised)"
					bind:value={color}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				/>
				<code class="muted text-xs">{color}</code>
				<Button size="sm" variant="ghost" onclick={() => (color = DEFAULT_COLOR)}>
					{t('common.reset')}
				</Button>
			</div>
		{/snippet}
	</Field>

	<Field
		label={t('form.description')}
		id="tag-description"
		optional
		error={fieldErrors['/description']}
	>
		{#snippet children({ id, describedBy, invalid })}
			<textarea
				{id}
				class="field"
				rows="2"
				maxlength="2000"
				bind:value={description}
				aria-describedby={describedBy}
				aria-invalid={invalid}
			></textarea>
		{/snippet}
	</Field>

	<div class="flex gap-2">
		<Button type="submit" variant="primary" loading={saving}>
			{editing ? t('common.save') : t('common.create')}
		</Button>
		<Button variant="ghost" onclick={oncancel}>{t('common.cancel')}</Button>
	</div>
</form>
