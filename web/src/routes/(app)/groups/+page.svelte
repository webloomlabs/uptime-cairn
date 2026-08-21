<script lang="ts">
	import { api } from '$lib/api';
	import type { Group, Tag, Page as ApiPage } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Button from '$lib/components/Button.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import StatusBadge from '$lib/components/StatusBadge.svelte';
	import GroupForm from '$lib/components/GroupForm.svelte';
	import TagForm from '$lib/components/TagForm.svelte';

	/**
	 * Groups and tags, which until now could be assigned everywhere and created
	 * nowhere.
	 *
	 * Both were readable and selectable across the dashboard — the monitor form's
	 * pickers, the list filters, the bulk bar's add-tag — and every one of those
	 * controls hides itself when its list is empty. With no way to create either,
	 * a fresh install had them permanently invisible and no way to find out why.
	 *
	 * They share a screen because they are the same job seen twice: a group is
	 * exclusive and hierarchical, a tag is many-to-many and flat, and an operator
	 * organising their monitors is choosing between them rather than visiting two
	 * places.
	 */
	let groups = $state<Group[]>([]);
	let tags = $state<Tag[]>([]);
	let loading = $state(true);
	let error = $state<unknown>(null);

	let editingGroup = $state<Group | null>(null);
	let creatingGroup = $state(false);
	let editingTag = $state<Tag | null>(null);
	let creatingTag = $state(false);

	const canReadGroups = $derived(session.allows('groups:read'));
	const canWriteGroups = $derived(session.allows('groups:write'));
	const canReadTags = $derived(session.allows('tags:read'));
	const canWriteTags = $derived(session.allows('tags:write'));

	/**
	 * Parents first, each followed by its own children. Nesting is one level deep
	 * by construction, so this is a sort rather than a tree walk — and doing it
	 * here keeps the markup a flat list, which is what a screen reader wants.
	 */
	const orderedGroups = $derived.by(() => {
		const roots = groups.filter((group) => group.parent_group_id === null);
		const out: { group: Group; nested: boolean }[] = [];
		for (const root of roots) {
			out.push({ group: root, nested: false });
			for (const child of groups) {
				if (child.parent_group_id === root.id) out.push({ group: child, nested: true });
			}
		}
		// Anything whose parent is outside the page we loaded still has to appear.
		// Dropping it would be a group that exists and cannot be edited.
		for (const group of groups) {
			if (!out.some((entry) => entry.group.id === group.id)) out.push({ group, nested: false });
		}
		return out;
	});

	async function load() {
		loading = true;
		error = null;
		try {
			if (canReadGroups) {
				groups = (await api.get<ApiPage<Group>>('/groups?limit=200')).data;
			}
			if (canReadTags) {
				tags = (await api.get<ApiPage<Tag>>('/tags?limit=200')).data;
			}
		} catch (caught) {
			error = caught;
		} finally {
			loading = false;
		}
	}

	async function removeGroup(group: Group) {
		if (!confirm(t('taxonomy.confirmDeleteGroup', { name: group.name }))) return;
		try {
			await api.delete(`/groups/${group.id}`);
			await load();
		} catch (caught) {
			error = caught;
		}
	}

	async function removeTag(tag: Tag) {
		if (!confirm(t('taxonomy.confirmDeleteTag', { name: tag.name }))) return;
		try {
			await api.delete(`/tags/${tag.id}`);
			await load();
		} catch (caught) {
			error = caught;
		}
	}

	function closeForms() {
		creatingGroup = false;
		editingGroup = null;
		creatingTag = false;
		editingTag = null;
	}

	$effect(() => {
		void load();
	});
</script>

<PageTitle title={t('nav.taxonomy')} />

<div class="space-y-8">
	{#if error}
		<ErrorBox {error} onretry={load} />
	{/if}

	{#if loading}
		<Spinner />
	{:else}
		{#if canReadGroups}
			<section class="space-y-3">
				<div class="flex flex-wrap items-center justify-between gap-3">
					<div>
						<h2 class="text-lg font-semibold">{t('taxonomy.groups')}</h2>
						<p class="muted text-sm">{t('taxonomy.groupsHint')}</p>
					</div>
					{#if canWriteGroups && !creatingGroup && !editingGroup}
						<Button
							variant="primary"
							onclick={() => {
								closeForms();
								creatingGroup = true;
							}}
						>
							<Icon name="plus" size={16} />
							{t('taxonomy.newGroup')}
						</Button>
					{/if}
				</div>

				{#if creatingGroup || editingGroup}
					<div class="card p-6">
						<h3 class="mb-4 font-semibold">
							{editingGroup ? editingGroup.name : t('taxonomy.newGroup')}
						</h3>
						{#key editingGroup?.id ?? 'new'}
							<GroupForm
								group={editingGroup}
								{groups}
								onsaved={() => {
									closeForms();
									void load();
								}}
								oncancel={closeForms}
							/>
						{/key}
					</div>
				{/if}

				{#if groups.length === 0}
					<p class="muted card px-4 py-10 text-center text-sm">{t('taxonomy.noGroups')}</p>
				{:else}
					<ul class="card divide-y" style="border-color: var(--border)">
						{#each orderedGroups as entry (entry.group.id)}
							<li
								class="flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-3"
								style="border-color: var(--border)"
							>
								<div class="min-w-0 flex-1" class:pl-6={entry.nested}>
									<p class="flex flex-wrap items-center gap-2 font-medium">
										{#if entry.nested}
											<span class="muted" aria-hidden="true">↳</span>
										{/if}
										{entry.group.name}
										{#if entry.group.status}
											<StatusBadge status={entry.group.status} size="sm" />
										{/if}
									</p>
									<p class="muted text-xs">
										{t('taxonomy.monitorCount', { count: entry.group.monitor_count })}
									</p>
									{#if entry.group.description}
										<p class="muted mt-1 text-xs">{entry.group.description}</p>
									{/if}
								</div>

								{#if canWriteGroups}
									<div class="flex gap-2">
										<Button
											size="sm"
											onclick={() => {
												closeForms();
												editingGroup = entry.group;
											}}
										>
											{t('common.edit')}
										</Button>
										<Button size="sm" variant="danger" onclick={() => removeGroup(entry.group)}>
											{t('common.delete')}
										</Button>
									</div>
								{/if}
							</li>
						{/each}
					</ul>
				{/if}
			</section>
		{/if}

		{#if canReadTags}
			<section class="space-y-3">
				<div class="flex flex-wrap items-center justify-between gap-3">
					<div>
						<h2 class="text-lg font-semibold">{t('taxonomy.tags')}</h2>
						<p class="muted text-sm">{t('taxonomy.tagsHint')}</p>
					</div>
					{#if canWriteTags && !creatingTag && !editingTag}
						<Button
							variant="primary"
							onclick={() => {
								closeForms();
								creatingTag = true;
							}}
						>
							<Icon name="plus" size={16} />
							{t('taxonomy.newTag')}
						</Button>
					{/if}
				</div>

				{#if creatingTag || editingTag}
					<div class="card p-6">
						<h3 class="mb-4 font-semibold">
							{editingTag ? editingTag.name : t('taxonomy.newTag')}
						</h3>
						{#key editingTag?.id ?? 'new'}
							<TagForm
								tag={editingTag}
								onsaved={() => {
									closeForms();
									void load();
								}}
								oncancel={closeForms}
							/>
						{/key}
					</div>
				{/if}

				{#if tags.length === 0}
					<p class="muted card px-4 py-10 text-center text-sm">{t('taxonomy.noTags')}</p>
				{:else}
					<ul class="card divide-y" style="border-color: var(--border)">
						{#each tags as tag (tag.id)}
							<li
								class="flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-3"
								style="border-color: var(--border)"
							>
								<!-- The swatch is decorative: the name is right beside it and
								     carries the meaning, so it is not given a label that would
								     be read out twice. -->
								<span
									class="h-4 w-4 shrink-0 rounded-full"
									style="background-color: {tag.color ?? '#6b7280'}"
									aria-hidden="true"
								></span>
								<div class="min-w-0 flex-1">
									<p class="font-medium">{tag.name}</p>
									<p class="muted text-xs">
										<code>{tag.slug}</code>
										· {t('taxonomy.monitorCount', { count: tag.monitor_count })}
									</p>
									{#if tag.description}
										<p class="muted mt-1 text-xs">{tag.description}</p>
									{/if}
								</div>

								{#if canWriteTags}
									<div class="flex gap-2">
										<Button
											size="sm"
											onclick={() => {
												closeForms();
												editingTag = tag;
											}}
										>
											{t('common.edit')}
										</Button>
										<Button size="sm" variant="danger" onclick={() => removeTag(tag)}>
											{t('common.delete')}
										</Button>
									</div>
								{/if}
							</li>
						{/each}
					</ul>
				{/if}
			</section>
		{/if}
	{/if}
</div>
