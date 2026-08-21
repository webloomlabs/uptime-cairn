<script lang="ts">
	import { api } from '$lib/api';
	import type { Page as ApiPage } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';

	/**
	 * Live template preview.
	 *
	 * The two things that make it worth having:
	 *
	 * The variable list is fetched, never hardcoded. `/notification-channels/
	 * template-variables` publishes exactly what the renderer resolves against,
	 * which is what stops an autocomplete drifting from the thing it completes —
	 * a list maintained in two places is a list that disagrees with itself within
	 * a release.
	 *
	 * The preview renders through the same server-side renderer delivery uses. A
	 * preview implemented in the client would be a second renderer, and a preview
	 * that renders through different code than delivery is a preview that lies at
	 * exactly the moment somebody is trusting it.
	 */
	let { template, label }: { template: string; label: string } = $props();

	type Variable = { name: string; type: string; description: string; example: unknown };
	type Preview = {
		ok: boolean;
		rendered_body: string | null;
		error: { message: string; line: number | null; column: number | null } | null;
	};

	let variables = $state<Variable[]>([]);
	let preview = $state<Preview | null>(null);
	let pending = $state(false);

	$effect(() => {
		(async () => {
			try {
				const result = await api.get<ApiPage<Variable> | Variable[]>(
					'/notification-channels/template-variables'
				);
				variables = Array.isArray(result) ? result : result.data;
			} catch {
				variables = [];
			}
		})();
	});

	$effect(() => {
		const current = template;
		if (!current.trim()) {
			preview = null;
			return;
		}

		// Debounced: this renders on the server, and rendering on every keystroke
		// would be a request per character.
		pending = true;
		const timer = setTimeout(async () => {
			try {
				preview = await api.post<Preview>('/notification-channels/preview', {
					template: current,
					event: 'monitor.down'
				});
			} catch {
				preview = null;
			} finally {
				pending = false;
			}
		}, 400);

		return () => {
			clearTimeout(timer);
			pending = false;
		};
	});
</script>

<div class="surface space-y-2 rounded-lg p-3">
	<p class="text-sm font-medium">{label} — preview</p>

	{#if pending && !preview}
		<p class="muted text-sm">{t('common.loading')}</p>
	{:else if preview?.ok && preview.rendered_body !== null}
		<pre
			class="overflow-x-auto rounded-md p-3 text-xs"
			style="background-color: var(--surface-sunken)">{preview.rendered_body}</pre>
	{:else if preview?.error}
		<!-- A broken template is a typo, shown inline. The server answers 200 with
		     ok:false for exactly this reason: it is the author's mistake, not a
		     server fault, and it should read like one. -->
		<div class="rounded-md px-3 py-2 text-sm" style="background-color: var(--color-down-soft)">
			{preview.error.message}
			{#if preview.error.line !== null}
				<span class="muted">
					(line {preview.error.line}{preview.error.column !== null
						? `, column ${preview.error.column}`
						: ''})
				</span>
			{/if}
		</div>
	{/if}

	{#if variables.length}
		<details>
			<summary class="muted cursor-pointer text-xs">
				{variables.length} variables available
			</summary>
			<ul class="mt-2 grid gap-x-4 gap-y-1 text-xs sm:grid-cols-2">
				{#each variables as variable (variable.name)}
					<li>
						<code class="font-mono">{`{{${variable.name}}}`}</code>
						<span class="muted"> — {variable.description}</span>
					</li>
				{/each}
			</ul>
		</details>
	{/if}
</div>
