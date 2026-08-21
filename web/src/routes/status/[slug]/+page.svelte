<script lang="ts">
	import { page } from '$app/state';
	import StatusPageView from '$lib/components/StatusPageView.svelte';

	/**
	 * `{base_url}/status/{slug}` — the address subscriber mail links to, and the
	 * one that has to keep working because a link in an inbox cannot be reissued
	 * (docs/api/README.md).
	 *
	 * The page itself is a component, because a custom-domain page renders the
	 * same thing at the bare root of a different hostname. Keyed on the slug so
	 * navigating from one page to another remounts rather than leaving the first
	 * one's state behind.
	 */
	const slug = $derived(page.params.slug!);
</script>

{#key slug}
	<StatusPageView {slug} />
{/key}
