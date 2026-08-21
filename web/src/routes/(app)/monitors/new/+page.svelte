<script lang="ts">
	import { t } from '$lib/i18n/index.svelte';
	import MonitorForm from '$lib/components/MonitorForm.svelte';
</script>

<div class="space-y-5">
	<div>
		<a href="/monitors" class="muted text-sm underline">{t('nav.monitors')}</a>
		<h1 class="mt-1 text-2xl font-semibold">{t('monitors.new')}</h1>
	</div>
	<MonitorForm />
</div>
