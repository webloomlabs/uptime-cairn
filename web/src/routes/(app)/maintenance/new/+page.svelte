<script lang="ts">
	import { page } from '$app/state';
	import { t } from '$lib/i18n/index.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import MaintenanceForm from '$lib/components/MaintenanceForm.svelte';

	// `?monitor=<id>` pre-fills a monitor, so "maintenance for this one" from a
	// monitor's own page is a single click away.
	const monitorIds = page.url.searchParams.getAll('monitor');
</script>

<PageTitle title={t('maintenance.new')} />
<MaintenanceForm {monitorIds} />
