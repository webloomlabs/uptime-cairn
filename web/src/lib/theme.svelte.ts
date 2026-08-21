import { browser } from '$app/environment';

export type Theme = 'light' | 'dark' | 'system';

const KEY = 'cairn.theme';

/**
 * Dark mode.
 *
 * Three states rather than two, because "follow the system" is a real choice and
 * the common one: an operator whose laptop switches at sunset expects the
 * dashboard to switch with it. The stored value is only ever the *override* —
 * 'system' clears the key rather than recording a third string, so a browser
 * that has never been told anything and one told to follow the system behave
 * identically.
 *
 * The class is applied here and also, before first paint, by the inline script
 * in app.html. Both are needed: the script prevents the flash on load, this
 * handles the change afterwards.
 */
class ThemeState {
	preference = $state<Theme>('system');
	/** What is actually on screen, after resolving 'system'. */
	resolved = $state<'light' | 'dark'>('light');

	constructor() {
		if (!browser) return;

		const saved = localStorage.getItem(KEY);
		this.preference = saved === 'dark' || saved === 'light' ? saved : 'system';

		const query = window.matchMedia('(prefers-color-scheme: dark)');
		query.addEventListener('change', () => {
			if (this.preference === 'system') this.apply();
		});
		this.apply();
	}

	set(preference: Theme): void {
		this.preference = preference;
		if (!browser) return;
		if (preference === 'system') localStorage.removeItem(KEY);
		else localStorage.setItem(KEY, preference);
		this.apply();
	}

	/** Cycles light → dark → system, for a single toggle button. */
	cycle(): void {
		const order: Theme[] = ['light', 'dark', 'system'];
		this.set(order[(order.indexOf(this.preference) + 1) % order.length]);
	}

	private apply(): void {
		const dark =
			this.preference === 'dark' ||
			(this.preference === 'system' && window.matchMedia('(prefers-color-scheme: dark)').matches);
		this.resolved = dark ? 'dark' : 'light';
		document.documentElement.classList.toggle('dark', dark);
	}
}

export const theme = new ThemeState();
