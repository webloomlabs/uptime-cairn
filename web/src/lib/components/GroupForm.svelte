<script lang="ts">
	import { untrack } from 'svelte';
	import { api, ApiError } from '$lib/api';
	import type { Group } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import Button from './Button.svelte';
	import Field from './Field.svelte';
	import ErrorBox from './ErrorBox.svelte';

	/**
	 * Create or edit a group.
	 *
	 * One form for both, like every other write surface here. The only thing
	 * specific to this resource is the parent picker, and the rule behind it is
	 * worth stating rather than discovering from a 422: groups nest exactly one
	 * level in this release, so the candidates are the top-level groups only, and
	 * a group that already has children cannot itself be nested
	 * (internal/api/taxonomy.go). Both are enforced by the server; the picker
	 * exists so the user is not offered the option that will be refused.
	 */
	let {
		group = null,
		groups = [],
		onsaved,
		oncancel
	}: {
		group?: Group | null;
		groups?: Group[];
		onsaved: () => void;
		oncancel: () => void;
	} = $props();

	const editing = $derived(Boolean(group));

	// Captured once, deliberately: the caller remounts this form with {#key} when
	// it switches record, so re-reading the prop would only overwrite what the
	// user has typed.
	const seed = untrack(() => ({
		name: group?.name ?? '',
		description: group?.description ?? '',
		parentId: group?.parent_group_id ?? ''
	}));

	let name = $state(seed.name);
	let description = $state(seed.description);
	let parentId = $state(seed.parentId);

	let saving = $state(false);
	let error = $state<unknown>(null);
	let fieldErrors = $state<Record<string, string>>({});

	// A group with children underneath it cannot be given a parent, so the picker
	// is withheld entirely rather than offering choices the server will refuse.
	const hasChildren = $derived(
		Boolean(group) && groups.some((candidate) => candidate.parent_group_id === group!.id)
	);

	const parentCandidates = $derived(
		groups.filter((candidate) => candidate.id !== group?.id && candidate.parent_group_id === null)
	);

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		saving = true;
		error = null;
		fieldErrors = {};

		// parent_group_id is sent as an explicit null rather than omitted: absent
		// leaves the parent alone and null promotes the group to the top level,
		// and clearing the picker means the second one.
		const body = {
			name,
			description: description.trim() === '' ? null : description,
			parent_group_id: parentId === '' ? null : parentId
		};

		try {
			if (editing) await api.patch(`/groups/${group!.id}`, body);
			else await api.post('/groups', body);
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

	<Field label={t('form.name')} id="group-name" error={fieldErrors['/name']}>
		{#snippet children({ id, describedBy, invalid })}
			<input
				{id}
				class="field"
				bind:value={name}
				maxlength="200"
				required
				aria-describedby={describedBy}
				aria-invalid={invalid}
			/>
		{/snippet}
	</Field>

	<Field
		label={t('form.description')}
		id="group-description"
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

	{#if hasChildren}
		<p class="muted text-sm">{t('taxonomy.hasChildren')}</p>
	{:else if parentCandidates.length}
		<Field
			label={t('taxonomy.parent')}
			id="group-parent"
			optional
			hint={t('taxonomy.parentHint')}
			error={fieldErrors['/parent_group_id']}
		>
			{#snippet children({ id, describedBy, invalid })}
				<select
					{id}
					class="field"
					bind:value={parentId}
					aria-describedby={describedBy}
					aria-invalid={invalid}
				>
					<option value="">{t('taxonomy.noParent')}</option>
					{#each parentCandidates as candidate (candidate.id)}
						<option value={candidate.id}>{candidate.name}</option>
					{/each}
				</select>
			{/snippet}
		</Field>
	{/if}

	<div class="flex gap-2">
		<Button type="submit" variant="primary" loading={saving}>
			{editing ? t('common.save') : t('common.create')}
		</Button>
		<Button variant="ghost" onclick={oncancel}>{t('common.cancel')}</Button>
	</div>
</form>
