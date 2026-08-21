<script lang="ts">
	import type { Snippet } from 'svelte';

	let {
		variant = 'secondary',
		size = 'md',
		type = 'button',
		disabled = false,
		loading = false,
		href = undefined,
		onclick = undefined,
		title = undefined,
		class: extra = '',
		children,
		...rest
	}: {
		variant?: 'primary' | 'secondary' | 'ghost' | 'danger';
		size?: 'sm' | 'md';
		type?: 'button' | 'submit';
		disabled?: boolean;
		loading?: boolean;
		href?: string;
		onclick?: (event: MouseEvent) => void;
		title?: string;
		class?: string;
		children?: Snippet;
		[key: string]: unknown;
	} = $props();

	const base =
		'inline-flex items-center justify-center gap-2 rounded-lg font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-50';
	const sizes = { sm: 'px-2.5 py-1.5 text-xs', md: 'px-4 py-2 text-sm' };
	const variants = {
		primary: 'text-[var(--accent-contrast)] bg-[var(--accent)] hover:bg-[var(--accent-hover)]',
		secondary:
			'border border-[var(--border)] bg-[var(--surface-raised)] hover:bg-[var(--surface-hover)]',
		ghost: 'hover:bg-[var(--surface-hover)]',
		danger:
			'border border-[var(--border)] text-[var(--color-down)] hover:bg-[var(--color-down-soft)]'
	};
	const classes = $derived(`${base} ${sizes[size]} ${variants[variant]} ${extra}`);
</script>

{#if href}
	<a {href} class={classes} {title} {...rest}>{@render children?.()}</a>
{:else}
	<button {type} class={classes} disabled={disabled || loading} {onclick} {title} {...rest}>
		{#if loading}
			<span
				class="h-3.5 w-3.5 animate-spin rounded-full border-2 border-current border-t-transparent"
				aria-hidden="true"
			></span>
		{/if}
		{@render children?.()}
	</button>
{/if}
