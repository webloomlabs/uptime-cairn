<script lang="ts">
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import type { Monitor } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import MonitorForm from '$lib/components/MonitorForm.svelte';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';

	const id = $derived(page.params.id!);

	let monitor = $state<Monitor | null>(null);
	let error = $state<unknown>(null);

	async function load() {
		error = null;
		try {
			// The read comes back with credentials replaced by a redaction marker,
			// and the form submits that marker untouched: the server resolves it
			// against the stored value. Loading the monitor is therefore safe even
			// though it is about to be written straight back.
			monitor = await api.get<Monitor>(`/monitors/${id}`);
		} catch (caught) {
			error = caught;
		}
	}

	$effect(() => {
		void id;
		void load();
	});
</script>

{#if error}
	<ErrorBox {error} onretry={load} />
{:else if !monitor}
	<Spinner />
{:else}
	<div class="space-y-5">
		<div>
			<a href="/monitors/{id}" class="muted text-sm underline">{monitor.name}</a>
			<h1 class="mt-1 text-2xl font-semibold">{t('common.edit')}</h1>
		</div>
		{#key monitor.id}
			<MonitorForm {monitor} />
		{/key}
	</div>
{/if}
