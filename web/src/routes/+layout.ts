/**
 * The whole dashboard is a client-side application served as static files from
 * the Go binary. There is no Node process in production (web/README.md), so
 * nothing can be server-rendered, and nothing is prerendered because every page
 * needs a live API.
 */
export const ssr = false;
export const prerender = false;
export const trailingSlash = 'never';
