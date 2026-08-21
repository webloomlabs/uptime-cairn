import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vite';

// Vite's config runs in Node, but the project carries no Node type package —
// declaring the one global this file touches is cheaper than a dependency whose
// only job is to describe it.
declare const process: { env: Record<string, string | undefined> };

export default defineConfig({
	plugins: [tailwindcss(), sveltekit()],
	server: {
		// `npm run dev` serves the UI; the API still comes from a locally running
		// cairn, so the dev server proxies /api to it rather than the frontend
		// keeping its own notion of where the backend lives. In production the two
		// are the same origin by construction — one binary — and this proxy is what
		// makes development match that.
		proxy: {
			'/api': {
				// 3000 is cairn's own default -listen port, so the two agree with no
				// flags on either side.
				target: process.env.CAIRN_URL ?? 'http://localhost:3000',
				changeOrigin: false
			}
		}
	}
});
