<script lang="ts">
	import { api, ApiError } from '$lib/api';
	import { t } from '$lib/i18n/index.svelte';
	import Spinner from './Spinner.svelte';
	import ErrorBox from './ErrorBox.svelte';

	/**
	 * Preview of a rendered HTML report artifact.
	 *
	 * # It reads the artifact endpoint rather than a preview endpoint
	 *
	 * There is no preview operation in the frozen spec and none is invented here
	 * (AGENTS.md rule 4). The existing artifact download answers with the exact
	 * bytes that were filed, and `Content-Disposition: attachment` governs
	 * top-level navigation rather than `fetch` — so the same URL a download link
	 * points at serves the preview, and the two cannot disagree about what the
	 * report says.
	 *
	 * **The stored artifact, never a re-render.** That is the same rule the share
	 * link follows and it matters for the same reason: a preview that re-rendered
	 * would show figures that had moved since the client was sent theirs, which
	 * makes the preview useless for the one question people ask it — *what did we
	 * actually send?*
	 *
	 * # The frame is sandboxed, and that is the whole design
	 *
	 * The document is injected with `srcdoc` into an iframe carrying a bare
	 * `sandbox` attribute — no `allow-scripts`, no `allow-same-origin`, no
	 * `allow-forms`, no `allow-top-navigation`. An empty sandbox is a *unique
	 * opaque origin*: the frame cannot reach this page's DOM, cannot read the
	 * session cookie, cannot run script, and cannot navigate the tab away.
	 *
	 * That matters because a report is not wholly ours. The renderer escapes what
	 * it writes, but the document carries a brand's company name and footer, and
	 * monitor names, subjects and issuers that came from somebody's configuration
	 * or off the wire from a probed certificate. Rendering that in this origin
	 * would make an escaping bug anywhere in the renderer into a session
	 * compromise here. Under the sandbox it stays a rendering bug.
	 *
	 * Inline CSS still applies under an empty sandbox — only script, forms and
	 * navigation are withheld — and the report artifacts are self-contained by
	 * construction: inline styles, and the logo embedded as a data URI, because
	 * ADR-007 requires the HTML to stand alone.
	 *
	 * **Security-relevant and agent-written: this is the file to read first.**
	 */
	let { runID, artifactID, onclose }: { runID: string; artifactID: string; onclose: () => void } =
		$props();

	let html = $state<string | null>(null);
	let error = $state<unknown>(null);
	let loading = $state(true);

	/**
	 * Loaded on mount rather than on hover or on render of the row.
	 *
	 * A report is a whole document and the largest artifact on the run; fetching
	 * one per row on the chance somebody opens it would pull megabytes nobody
	 * asked for. The panel is only mounted once the control is pressed, so this
	 * runs exactly when the question was asked.
	 */
	$effect(() => {
		let cancelled = false;
		const controller = new AbortController();

		(async () => {
			try {
				const body = await api.text(`/report-runs/${runID}/artifacts/${artifactID}`, {
					signal: controller.signal
				});
				if (!cancelled) html = body;
			} catch (cause) {
				if (cause instanceof DOMException && cause.name === 'AbortError') return;
				if (!cancelled) error = cause;
			} finally {
				if (!cancelled) loading = false;
			}
		})();

		return () => {
			cancelled = true;
			controller.abort();
		};
	});

	/**
	 * A 410 here is the tombstone case and reads as one.
	 *
	 * The row offered a preview because the artifact said `rendered` with a
	 * download link, and the bytes went missing between that read and this one —
	 * retention reclaiming them, or a database restored without its reports
	 * directory. The server's own sentence is shown rather than replaced, exactly
	 * as the download path does it, so the two cannot drift.
	 */
	const gone = $derived(error instanceof ApiError && error.status === 410);
</script>

<div class="mt-2 rounded border" style="border-color: var(--border)">
	<div
		class="flex items-center justify-between border-b px-3 py-2"
		style="border-color: var(--border)"
	>
		<h4 class="text-xs font-medium">{t('runs.previewTitle')}</h4>
		<button type="button" class="text-xs hover:underline" onclick={onclose}>
			{t('runs.previewClose')}
		</button>
	</div>

	{#if loading}
		<div class="p-4"><Spinner /></div>
	{:else if error}
		<div class="p-3">
			<ErrorBox {error} />
			{#if gone}
				<p class="muted mt-2 text-xs">{t('runs.previewGoneHint')}</p>
			{/if}
		</div>
	{:else if html}
		<!--
			`sandbox` with no tokens at all. Adding one here — `allow-scripts` in
			particular — would undo the reason this element is safe to point at a
			document carrying somebody else's brand text and a probed certificate's
			subject. The report needs none of them.

			`title` is required for the frame to be reachable by a screen reader,
			which would otherwise announce it as an unlabelled region.
		-->
		<iframe
			sandbox=""
			srcdoc={html}
			title={t('runs.previewTitle')}
			class="h-[70vh] w-full rounded-b bg-white"
		></iframe>
	{/if}
</div>
