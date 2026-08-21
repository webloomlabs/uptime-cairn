import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/**
 * The dashboard compiles to static files that Go embeds and serves; there is no
 * Node process in production (web/README.md). That makes adapter-static the only
 * adapter that fits, and `fallback` the load-bearing option: every route is
 * resolved in the browser, so the server has to answer an unknown deep path with
 * index.html rather than 404. `/status/acme` and the two subscription links in
 * subscriber mail are exactly those paths.
 *
 * Output lands directly in internal/ui/dist, which is generated. See
 * internal/ui/embed.go for why a placeholder is committed there.
 */
const config = {
	preprocess: vitePreprocess(),
	kit: {
		adapter: adapter({
			pages: '../internal/ui/dist',
			assets: '../internal/ui/dist',
			fallback: 'index.html',
			precompress: false,
			strict: true
		}),
		// Nothing is prerendered and nothing is server-rendered: the binary serves
		// bytes, and every page's data comes from /api/v1 like any other client.
		prerender: { entries: [] },
		typescript: {
			config: (c) => {
				c.include.push('../vite.config.ts');
				return c;
			}
		}
	}
};

export default config;
