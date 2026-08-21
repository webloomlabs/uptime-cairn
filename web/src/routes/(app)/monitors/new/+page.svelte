<script lang="ts">
	import { t } from '$lib/i18n/index.svelte';
	import MonitorForm from '$lib/components/MonitorForm.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import Icon from '$lib/components/Icon.svelte';
</script>

<div class="space-y-5">
	<a
		href="/"
		class="muted inline-flex items-center gap-1 rounded-lg px-2.5 py-1.5 text-sm transition-colors hover:bg-[var(--surface-hover)]"
	>
		<Icon name="chevronLeft" size={16} />
		{t('nav.monitoring')}
	</a>
	<PageTitle title={t('monitors.new')} />
	<MonitorForm />
</div>
