<script lang="ts">
	import { goto } from '$app/navigation';
	import { api, ApiError } from '$lib/api';
	import type { Session } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import Button from '$lib/components/Button.svelte';
	import Field from '$lib/components/Field.svelte';
	import ThemeToggle from '$lib/components/ThemeToggle.svelte';

	/**
	 * First run: the one account that creates itself.
	 *
	 * The window closes the moment it succeeds, and the server is what closes it —
	 * this page redirects away when `setup_required` is false rather than deciding
	 * anything on its own. The timezone is pre-filled from the browser because it
	 * is the one field somebody cannot get wrong by accident and would otherwise
	 * have to look up.
	 */
	let instanceName = $state('Uptime Cairn');
	let name = $state('');
	let email = $state('');
	let password = $state('');
	let timezone = $state(Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC');
	let busy = $state(false);
	let message = $state<string | null>(null);
	let fieldErrors = $state<Record<string, string>>({});

	$effect(() => {
		if (!session.setupRequired) goto('/', { replaceState: true });
	});

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		busy = true;
		message = null;
		fieldErrors = {};

		try {
			const result = await api.post<Session>(
				'/setup',
				{ email, name, password, instance_name: instanceName, timezone },
				{ expectUnauthorised: true }
			);
			session.setupRequired = false;
			session.adopt(result);
			await session.loadInfo();
			await goto('/', { replaceState: true });
		} catch (caught) {
			if (caught instanceof ApiError) {
				fieldErrors = caught.fields();
				message = caught.problem.detail || caught.problem.title;
			} else {
				message = t('error.unexpected');
			}
		} finally {
			busy = false;
		}
	}

	const inputClass = 'w-full rounded-md border px-3 py-2 text-sm';
	const inputStyle = 'border-color: var(--border-strong); background-color: var(--surface)';
</script>

<div class="flex min-h-full items-center justify-center p-6">
	<div class="w-full max-w-md space-y-6">
		<div class="flex items-center justify-between">
			<h1 class="text-xl font-semibold">{t('app.name')}</h1>
			<ThemeToggle />
		</div>

		<form class="surface space-y-4 rounded-lg p-5" onsubmit={submit}>
			<div>
				<h2 class="font-semibold">{t('setup.title')}</h2>
				<p class="muted mt-1 text-sm">{t('setup.intro')}</p>
			</div>

			<Field label={t('setup.instanceName')} id="instance" error={fieldErrors['/instance_name']}>
				{#snippet children({ id })}
					<input {id} class={inputClass} style={inputStyle} bind:value={instanceName} required />
				{/snippet}
			</Field>

			<Field label={t('setup.yourName')} id="name" error={fieldErrors['/name']}>
				{#snippet children({ id })}
					<input
						{id}
						class={inputClass}
						style={inputStyle}
						bind:value={name}
						autocomplete="name"
						required
					/>
				{/snippet}
			</Field>

			<Field label={t('auth.email')} id="email" error={fieldErrors['/email']}>
				{#snippet children({ id })}
					<input
						{id}
						type="email"
						class={inputClass}
						style={inputStyle}
						bind:value={email}
						autocomplete="username"
						required
					/>
				{/snippet}
			</Field>

			<Field label={t('auth.password')} id="password" error={fieldErrors['/password']}>
				{#snippet children({ id })}
					<input
						{id}
						type="password"
						class={inputClass}
						style={inputStyle}
						bind:value={password}
						autocomplete="new-password"
						required
					/>
				{/snippet}
			</Field>

			<Field label={t('setup.timezone')} id="timezone" error={fieldErrors['/timezone']}>
				{#snippet children({ id })}
					<input {id} class={inputClass} style={inputStyle} bind:value={timezone} required />
				{/snippet}
			</Field>

			{#if message}
				<p class="text-sm" style="color: var(--color-down)" role="alert">{message}</p>
			{/if}

			<Button type="submit" variant="primary" loading={busy} class="w-full">
				{t('setup.submit')}
			</Button>
		</form>
	</div>
</div>
