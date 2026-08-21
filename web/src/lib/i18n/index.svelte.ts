import { browser } from '$app/environment';
import en from './en.json';

/**
 * i18n scaffolding.
 *
 * English is the source catalogue and the only one that ships complete; the
 * point of this file is that adding a language is a JSON file and a line in
 * `LOCALES`, never a change to a component. See web/README.md for the
 * translation pipeline.
 *
 * Three deliberate choices:
 *
 *   - **Flat, dotted keys, and the key is never the English text.** A catalogue
 *     keyed by its own source string breaks every translation the moment
 *     somebody fixes a typo in English.
 *   - **A missing key renders the key**, loudly, rather than falling back to
 *     English silently. A half-translated UI that looks finished is how a
 *     language ships with a third of its strings missing for two years.
 *   - **No plural machinery yet.** English needs two forms and a `{n}` counter
 *     covers it; a language with six is a real problem and deserves Intl.PluralRules
 *     rather than a guess made now. The catalogue keys are shaped so that adding
 *     it later does not rename anything.
 */

export type Catalogue = Record<string, string>;

/** Every locale this build carries. English is not lazy — it is the fallback. */
const LOCALES: Record<string, () => Promise<{ default: Catalogue }>> = {
	en: async () => ({ default: en as Catalogue })
};

export const availableLocales = Object.keys(LOCALES);

class I18nState {
	locale = $state('en');
	private catalogue = $state<Catalogue>(en as Catalogue);

	/**
	 * Picks the best available locale for a preference list, matching the
	 * language subtag: a browser asking for `en-AU` gets `en` rather than
	 * nothing, which is what makes regional variants free.
	 */
	negotiate(preferences: readonly string[]): string {
		for (const preference of preferences) {
			const tag = preference.toLowerCase();
			if (LOCALES[tag]) return tag;
			const language = tag.split('-')[0];
			if (LOCALES[language]) return language;
		}
		return 'en';
	}

	async use(locale: string): Promise<void> {
		const load = LOCALES[locale];
		if (!load) return;
		const module = await load();
		this.catalogue = module.default;
		this.locale = locale;
		if (browser) document.documentElement.lang = locale;
	}

	/** Adopts the browser's preference. Called once, from the root layout. */
	async detect(): Promise<void> {
		if (!browser) return;
		await this.use(this.negotiate(navigator.languages ?? [navigator.language]));
	}

	translate(key: string, values?: Record<string, string | number>): string {
		const template = this.catalogue[key];
		if (template === undefined) return key;
		if (!values) return template;
		return template.replace(/\{(\w+)\}/g, (whole, name: string) =>
			name in values ? String(values[name]) : whole
		);
	}
}

export const i18n = new I18nState();

/**
 * The translation function.
 *
 * A plain function rather than a store subscription, and it stays reactive
 * because it reads `i18n.catalogue` — a rune — during render. Changing locale
 * re-renders every component that called it, with no per-component wiring.
 */
export function t(key: string, values?: Record<string, string | number>): string {
	return i18n.translate(key, values);
}
