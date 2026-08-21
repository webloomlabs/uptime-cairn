<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, ApiError } from '$lib/api';
	import type { Session } from '$lib/types';
	import { t } from '$lib/i18n/index.svelte';
	import { session } from '$lib/session.svelte';
	import Button from '$lib/components/Button.svelte';
	import Field from '$lib/components/Field.svelte';
	import ThemeToggle from '$lib/components/ThemeToggle.svelte';

	/**
	 * Sign in, including the second factor.
	 *
	 * TOTP is not a separate page: the server answers the first attempt with a
	 * 401 whose type ends `/totp-required`, and that reveals the code field in
	 * place. Branching on the problem type rather than on the status code is what
	 * keeps this from also firing on an ordinary wrong password.
	 */
	let email = $state('');
	let password = $state('');
	let totpCode = $state('');
	let recoveryCode = $state('');
	let needsTOTP = $state(false);
	let useRecovery = $state(false);
	let busy = $state(false);
	let message = $state<string | null>(null);

	const expired = $derived(page.url.searchParams.get('expired') === '1');
	const next = $derived(page.url.searchParams.get('next') || '/');

	$effect(() => {
		if (session.setupRequired) goto('/setup', { replaceState: true });
		else if (session.authenticated) goto(next, { replaceState: true });
	});

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		busy = true;
		message = null;

		try {
			const body: Record<string, string> = { email, password };
			if (needsTOTP) {
				if (useRecovery) body.recovery_code = recoveryCode.trim();
				else body.totp_code = totpCode.trim();
			}

			const result = await api.post<Session>('/auth/login', body, {
				expectUnauthorised: true
			});
			session.reauthRequired = false;
			session.adopt(result);
			await session.loadInfo();
			await goto(next, { replaceState: true });
		} catch (caught) {
			if (caught instanceof ApiError && caught.is('totp-required')) {
				needsTOTP = true;
				message = null;
			} else if (caught instanceof ApiError) {
				message = caught.problem.detail || caught.problem.title;
			} else {
				message = t('auth.signInFailed');
			}
		} finally {
			busy = false;
		}
	}

	// Both defined in app.css, so every control in the product shares one
	// treatment rather than each form carrying its own copy of it.
	const inputClass = 'field';
	const inputStyle = '';
</script>

<div class="flex min-h-full items-center justify-center p-6">
	<div class="w-full max-w-sm space-y-6">
		<div class="flex items-center justify-between">
			<h1 class="text-xl font-semibold">{t('app.name')}</h1>
			<ThemeToggle />
		</div>

		{#if session.reauthRequired}
			<p class="rounded-md px-3 py-2 text-sm" style="background-color: var(--color-pending-soft)">
				{t('auth.reauthRequired')}
			</p>
		{:else if expired}
			<p class="rounded-md px-3 py-2 text-sm" style="background-color: var(--color-pending-soft)">
				{t('auth.sessionExpired')}
			</p>
		{/if}

		<form class="card space-y-4 p-6" onsubmit={submit}>
			<Field label={t('auth.email')} id="email">
				{#snippet children({ id })}
					<input
						{id}
						type="email"
						class={inputClass}
						style={inputStyle}
						bind:value={email}
						autocomplete="username"
						required
						readonly={needsTOTP}
					/>
				{/snippet}
			</Field>

			<Field label={t('auth.password')} id="password">
				{#snippet children({ id })}
					<input
						{id}
						type="password"
						class={inputClass}
						style={inputStyle}
						bind:value={password}
						autocomplete="current-password"
						required
						readonly={needsTOTP}
					/>
				{/snippet}
			</Field>

			{#if needsTOTP}
				{#if useRecovery}
					<Field label={t('auth.recoveryCodeLabel')} id="recovery">
						{#snippet children({ id })}
							<input
								{id}
								class="{inputClass} font-mono"
								style={inputStyle}
								bind:value={recoveryCode}
								autocomplete="one-time-code"
								required
							/>
						{/snippet}
					</Field>
				{:else}
					<Field label={t('auth.totpCode')} id="totp" hint={t('auth.totpHint')}>
						{#snippet children({ id, describedBy })}
							<!-- svelte-ignore a11y_autofocus -->
							<input
								{id}
								class="{inputClass} font-mono tracking-widest"
								style={inputStyle}
								bind:value={totpCode}
								inputmode="numeric"
								autocomplete="one-time-code"
								maxlength="6"
								autofocus
								required
								aria-describedby={describedBy}
							/>
						{/snippet}
					</Field>
				{/if}

				<button
					type="button"
					class="text-sm underline"
					onclick={() => (useRecovery = !useRecovery)}
				>
					{useRecovery ? t('auth.totpCode') : t('auth.recoveryCode')}
				</button>
			{/if}

			{#if message}
				<p class="text-sm" style="color: var(--color-down)" role="alert">{message}</p>
			{/if}

			<Button type="submit" variant="primary" loading={busy} class="w-full">
				{t('auth.signIn')}
			</Button>
		</form>
	</div>
</div>
