<script lang="ts">
	import { api } from '$lib/api';
	import type { NotificationChannel, Page as ApiPage } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import { channelSpec } from '$lib/channeltypes';
	import { formatRelative } from '$lib/format';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';
	import Button from '$lib/components/Button.svelte';
	import ChannelForm from '$lib/components/ChannelForm.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import Icon from '$lib/components/Icon.svelte';

	let channels = $state<NotificationChannel[]>([]);
	let loading = $state(true);
	let error = $state<unknown>(null);
	let editing = $state<NotificationChannel | null>(null);
	let creating = $state(false);

	const canWrite = $derived(session.allows('notifications:write'));

	async function load() {
		loading = true;
		error = null;
		try {
			channels = (await api.get<ApiPage<NotificationChannel>>('/notification-channels?limit=100'))
				.data;
		} catch (caught) {
			error = caught;
		} finally {
			loading = false;
		}
	}

	async function remove(channel: NotificationChannel) {
		if (!confirm(`Delete ${channel.name}?`)) return;
		try {
			await api.delete(`/notification-channels/${channel.id}`);
			await load();
		} catch (caught) {
			error = caught;
		}
	}

	$effect(() => {
		void load();
	});
</script>

<PageTitle title={t('nav.notifications')}>
	{#snippet actions()}
		{#if canWrite && !creating && !editing}
			<Button
				variant="primary"
				onclick={() => {
					creating = true;
					editing = null;
				}}
			>
				<Icon name="plus" size={16} />
				{t('notifications.newChannel')}
			</Button>
		{/if}
	{/snippet}
</PageTitle>

<div class="space-y-5">
	{#if creating || editing}
		<section class="card p-6">
			<h2 class="mb-4 font-semibold">
				{editing ? editing.name : t('notifications.newChannel')}
			</h2>
			{#key editing?.id ?? 'new'}
				<ChannelForm
					channel={editing}
					onsaved={() => {
						creating = false;
						editing = null;
						void load();
					}}
					oncancel={() => {
						creating = false;
						editing = null;
					}}
				/>
			{/key}
		</section>
	{/if}

	{#if error}
		<ErrorBox {error} onretry={load} />
	{/if}

	{#if loading}
		<Spinner />
	{:else if channels.length === 0}
		<p class="muted card px-4 py-12 text-center text-sm">No notification channels yet.</p>
	{:else}
		<ul class="card divide-y" style="border-color: var(--border)">
			{#each channels as channel (channel.id)}
				<li
					class="flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-3"
					style="border-color: var(--border)"
				>
					<div class="min-w-0 flex-1">
						<p class="flex flex-wrap items-center gap-2 font-medium">
							{channel.name}
							{#if !channel.enabled}
								<span class="muted text-xs">({t('status.paused')})</span>
							{/if}
							{#if channel.is_default}
								<span class="muted text-xs">· default</span>
							{/if}
						</p>
						<p class="muted text-xs">
							{channelSpec(channel.type)?.label ?? channel.type}
							{#if channel.last_success_at}
								· last used {formatRelative(channel.last_success_at)}
							{/if}
						</p>
						<!-- A channel that has silently stopped working is the one
						     failure mode this feature cannot have, so the last error
						     is on the channel itself rather than buried in a log. -->
						{#if channel.last_error}
							<p class="mt-1 text-xs" style="color: var(--color-down)">
								{channel.last_error}
							</p>
						{/if}
					</div>

					{#if canWrite}
						<div class="flex gap-2">
							<Button
								size="sm"
								onclick={() => {
									editing = channel;
									creating = false;
								}}
							>
								{t('common.edit')}
							</Button>
							<Button size="sm" variant="danger" onclick={() => remove(channel)}>
								{t('common.delete')}
							</Button>
						</div>
					{/if}
				</li>
			{/each}
		</ul>
	{/if}
</div>
