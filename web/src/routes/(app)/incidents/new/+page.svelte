<script lang="ts">
	import { t } from '$lib/i18n/index.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import IncidentForm from '$lib/components/IncidentForm.svelte';
</script>

<PageTitle title={t('incidents.new')} />
<IncidentForm />
