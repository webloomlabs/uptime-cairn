<script lang="ts">
	import { untrack } from 'svelte';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import type { Incident } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import IncidentForm from '$lib/components/IncidentForm.svelte';
	import Spinner from '$lib/components/Spinner.svelte';
	import ErrorBox from '$lib/components/ErrorBox.svelte';

	let incident = $state<Incident | null>(null);
	let error = $state<unknown>(null);

	async function load() {
		error = null;
		try {
			incident = await api.get<Incident>(`/incidents/${page.params.id}`);
		} catch (caught) {
			error = caught;
		}
	}

	$effect(() => {
		untrack(() => void load());
	});
</script>

<PageTitle title={t('incidents.edit')} />

{#if error}
	<ErrorBox {error} onretry={load} />
{:else if !incident}
	<Spinner />
{:else}
	<!-- Keyed on the id so navigating from one incident's edit screen to
	     another's remounts the form rather than leaving it holding the first
	     one's text. -->
	{#key incident.id}
		<IncidentForm {incident} />
	{/key}
{/if}
