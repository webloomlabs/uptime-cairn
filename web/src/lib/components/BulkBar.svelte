<script lang="ts">
	import { api, ApiError } from '$lib/api';
	import type { Tag } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import Button from './Button.svelte';

	/**
	 * Bulk operations over the current selection.
	 *
	 * The server's contract is partial success: every identifier gets its own
	 * outcome and one monitor deleted five minutes ago does not fail the other
	 * nine hundred and ninety-nine (internal/api/monitor_writes.go). So the result
	 * is reported as two numbers rather than as "done" — a bar that says success
	 * after failing a third of a batch is how somebody discovers next week that
	 * their tag never landed.
	 */
	let {
		ids,
		tags = [],
		onclear,
		ondone
	}: {
		ids: string[];
		tags?: Tag[];
		onclear: () => void;
		ondone: () => void;
	} = $props();

	type BulkResult = {
		succeeded: string[];
		failed: { id: string; code: string; message: string }[];
	};

	let running = $state<string | null>(null);
	let result = $state<BulkResult | null>(null);
	let error = $state<string | null>(null);
	let tagId = $state('');

	async function run(operation: string, body: Record<string, unknown> = {}) {
		if (operation === 'delete' && !confirm(t('bulk.applyTo', { count: ids.length }))) return;

		running = operation;
		error = null;
		result = null;
		try {
			result = await api.post<BulkResult>('/monitors/bulk', {
				monitor_ids: ids,
				operation,
				...body
			});
			// Deliberately not closing on completion: the counts are the point, and
			// a bar that vanishes takes the failure count with it.
			ondone();
		} catch (caught) {
			error =
				caught instanceof ApiError
					? caught.problem.detail || caught.problem.title
					: t('error.unexpected');
		} finally {
			running = null;
		}
	}
</script>

<div
	class="fixed inset-x-0 bottom-0 z-20 border-t px-4 py-3 sm:px-8 lg:pl-64"
	style="border-color: var(--border); background-color: var(--surface-raised)"
	role="region"
	aria-label={t('monitors.selected', { count: ids.length })}
>
	<div class="flex flex-wrap items-center gap-2">
		<span class="text-sm font-medium">{t('monitors.selected', { count: ids.length })}</span>

		<Button size="sm" loading={running === 'enable'} onclick={() => run('enable')}>
			{t('bulk.enable')}
		</Button>
		<Button size="sm" loading={running === 'disable'} onclick={() => run('disable')}>
			{t('bulk.disable')}
		</Button>

		{#if tags.length}
			<select class="field w-auto" bind:value={tagId}>
				<option value="">{t('form.tags')}…</option>
				{#each tags as tag (tag.id)}
					<option value={tag.id}>{tag.name}</option>
				{/each}
			</select>
			<Button
				size="sm"
				disabled={!tagId}
				loading={running === 'add_tags'}
				onclick={() => run('add_tags', { tag_ids: [tagId] })}
			>
				{t('bulk.addTag')}
			</Button>
			<Button
				size="sm"
				disabled={!tagId}
				loading={running === 'remove_tags'}
				onclick={() => run('remove_tags', { tag_ids: [tagId] })}
			>
				{t('bulk.removeTag')}
			</Button>
		{/if}

		<Button size="sm" variant="danger" loading={running === 'delete'} onclick={() => run('delete')}>
			{t('bulk.delete')}
		</Button>

		<div class="ml-auto flex items-center gap-3">
			{#if error}
				<span class="text-sm" style="color: var(--color-down)">{error}</span>
			{:else if result}
				<span class="text-sm" style={result.failed.length ? 'color: var(--color-down)' : ''}>
					{t('bulk.result', {
						succeeded: result.succeeded.length,
						failed: result.failed.length
					})}
				</span>
			{/if}
			<Button size="sm" variant="ghost" onclick={onclear}>{t('common.close')}</Button>
		</div>
	</div>
</div>
