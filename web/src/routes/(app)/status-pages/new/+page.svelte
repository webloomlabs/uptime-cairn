<script lang="ts">
	import { t } from '$lib/i18n/index.svelte';
	import StatusPageForm from '$lib/components/StatusPageForm.svelte';
	import PageTitle from '$lib/components/PageTitle.svelte';
	import Icon from '$lib/components/Icon.svelte';
</script>

<div class="space-y-5">
	<a
		href="/status-pages"
		class="muted inline-flex items-center gap-1 rounded-lg px-2.5 py-1.5 text-sm transition-colors hover:bg-[var(--surface-hover)]"
	>
		<Icon name="chevronLeft" size={16} />
		{t('nav.statusPages')}
	</a>
	<PageTitle title={t('statusPages.new')} />
	<StatusPageForm />
</div>
