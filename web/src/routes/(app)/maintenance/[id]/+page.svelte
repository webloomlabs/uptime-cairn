<script lang="ts">
	import { untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import type { MaintenanceWindow } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import MaintenanceForm from '$lib/components/MaintenanceForm.svelte';
	import Button from '$lib/components/Button.svelte';
	import Icon from '$lib/components/Icon.svelte';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';

	let maintenance = $state<MaintenanceWindow | null>(null);
	let error = $state<unknown>(null);
	let deleting = $state(false);

	async function load() {
		error = null;
		try {
			maintenance = await api.get<MaintenanceWindow>(`/maintenance-windows/${page.params.id}`);
		} catch (caught) {
			error = caught;
		}
	}

	$effect(() => {
		untrack(() => void load());
	});

	// Deleting is how the API cancels a window. The annotations it left on
	// monitor history stay, so past uptime figures do not move.
	async function remove() {
		if (!maintenance) return;
		if (!confirm(t('maintenance.deleteConfirm'))) return;
		deleting = true;
		try {
			await api.delete(`/maintenance-windows/${maintenance.id}`);
			await goto('/maintenance');
		} catch (caught) {
			error = caught;
		} finally {
			deleting = false;
		}
	}
</script>

<PageTitle title={t('maintenance.edit')}>
	{#snippet actions()}
		{#if maintenance && session.allows('maintenance:write')}
			<Button variant="danger" loading={deleting} disabled={deleting} onclick={remove}>
				<Icon name="trash" size={16} />
				{t('common.delete')}
			</Button>
		{/if}
	{/snippet}
</PageTitle>

{#if error}
	<ErrorBox {error} onretry={load} />
{:else if !maintenance}
	<Spinner />
{:else}
	<!-- Keyed on the id so moving between windows remounts the form. -->
	{#key maintenance.id}
		<MaintenanceForm {maintenance} />
	{/key}
{/if}
