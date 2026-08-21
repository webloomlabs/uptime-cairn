/**
 * The API client.
 *
 * The dashboard is an ordinary API client (web/README.md): it consumes /api/v1
 * and nothing else, with no privileged endpoint and no field it can set that a
 * scoped API key cannot. That rule is the reason this file is the only place in
 * the frontend that calls `fetch` — anything reaching around it would be reaching
 * for a back door that does not exist.
 *
 * Two things it is responsible for beyond transport:
 *
 *   - CSRF. Cookie authentication is ambient, so every write echoes the token
 *     the session response carried, in `X-Cairn-CSRF-Token`. Missing it is a 403
 *     from the server, not a silent success (internal/api/auth.go).
 *   - RFC 9457. Every failure is a problem document, and clients branch on
 *     `type` rather than on prose, because `title` and `detail` may be
 *     translated. `ApiError` keeps the URI and the per-field `errors` array so a
 *     form can highlight the field the server named.
 */

/** One invalid field, located by an RFC 6901 JSON pointer. */
export type ValidationItem = {
	pointer: string;
	code: string;
	message: string;
};

export type Problem = {
	type: string;
	title: string;
	status: number;
	detail?: string;
	instance?: string;
	errors?: ValidationItem[];
};

const ERROR_BASE = 'https://uptimecairn.dev/errors/';

export class ApiError extends Error {
	readonly status: number;
	readonly problem: Problem;

	constructor(problem: Problem) {
		super(problem.detail || problem.title);
		this.name = 'ApiError';
		this.status = problem.status;
		this.problem = problem;
	}

	/**
	 * The stable half of the contract. Compare against the short kind
	 * ('validation-failed', 'totp-required') rather than the full URI, so a
	 * change of documentation host does not become a change of behaviour.
	 */
	is(kind: string): boolean {
		return this.problem.type === ERROR_BASE + kind || this.problem.type.endsWith('/' + kind);
	}

	/** Validation messages keyed by JSON pointer, for direct use by a form. */
	fields(): Record<string, string> {
		const out: Record<string, string> = {};
		for (const item of this.problem.errors ?? []) out[item.pointer] = item.message;
		return out;
	}
}

/**
 * The CSRF token, and why it is persisted.
 *
 * The server issues it exactly once, in the response to `POST /auth/login` or
 * `POST /setup`. `GET /auth/session` deliberately does **not** return it —
 * internal/api/handlers_auth.go says so in as many words, because an endpoint
 * that reissues it on demand would let anything able to make a `GET` obtain the
 * token that authorises a write.
 *
 * That is a decision about the *server*, and it has a consequence for the client
 * that is easy to miss: there is no way to recover the token after the page
 * unloads. Keeping it in memory alone means every reload, and every newly opened
 * tab, holds a perfectly valid session cookie and cannot write with it — every
 * write answers 403 until the user signs in again.
 *
 * So it is persisted. `localStorage` rather than `sessionStorage` because a
 * second tab opened against the same session has the same problem and no login
 * to recover from. This does not weaken what the token defends against: CSRF is
 * a cross-origin attack, and no cross-origin page can read this origin's
 * storage. An attacker already executing script on this origin can mint requests
 * with the ambient cookie regardless of where the token is kept.
 *
 * It is cleared on sign-out and whenever the server rejects the session, so a
 * stale token does not outlive the cookie it pairs with.
 */
const CSRF_KEY = 'cairn.csrf';

let csrfToken: string | null = null;

export function setCSRFToken(token: string | null): void {
	csrfToken = token;
	try {
		if (token === null) localStorage.removeItem(CSRF_KEY);
		else localStorage.setItem(CSRF_KEY, token);
	} catch {
		// Private mode, or storage disabled. The token still works for this page;
		// it just will not survive a reload.
	}
}

/** Restores the token a previous page load was given. Called once, at startup. */
export function restoreCSRFToken(): void {
	try {
		csrfToken = localStorage.getItem(CSRF_KEY);
	} catch {
		csrfToken = null;
	}
}

export function getCSRFToken(): string | null {
	return csrfToken;
}

/** Called when the server says the session is gone, so the app can react once. */
type Unauthorised = () => void;
let onUnauthorised: Unauthorised = () => {};

export function handleUnauthorised(fn: Unauthorised): void {
	onUnauthorised = fn;
}

export type RequestOptions = {
	method?: string;
	body?: unknown;
	query?: Record<string, string | number | boolean | undefined | null>;
	signal?: AbortSignal;
	/** Suppresses the global session-expiry hook, for calls that expect a 401. */
	expectUnauthorised?: boolean;
};

const SAFE = new Set(['GET', 'HEAD', 'OPTIONS']);

export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
	const method = (options.method ?? 'GET').toUpperCase();
	const url = new URL(`/api/v1${path}`, window.location.origin);

	for (const [key, value] of Object.entries(options.query ?? {})) {
		// Absent and empty are the same thing for a filter, and both mean "do not
		// constrain": sending `status=` would be a filter on the empty string.
		if (value === undefined || value === null || value === '') continue;
		url.searchParams.set(key, String(value));
	}

	const headers: Record<string, string> = { Accept: 'application/json' };
	if (options.body !== undefined) headers['Content-Type'] = 'application/json';
	if (!SAFE.has(method) && csrfToken) headers['X-Cairn-CSRF-Token'] = csrfToken;

	let response: Response;
	try {
		response = await fetch(url, {
			method,
			headers,
			credentials: 'same-origin',
			signal: options.signal,
			body: options.body === undefined ? undefined : JSON.stringify(options.body)
		});
	} catch (cause) {
		if (cause instanceof DOMException && cause.name === 'AbortError') throw cause;
		// A dead network and a dead server are indistinguishable from here, and
		// saying so is more useful than inventing a status code for it.
		throw new ApiError({
			type: ERROR_BASE + 'network',
			title: 'Cannot reach the server',
			status: 0,
			detail:
				'The request did not complete. The server may be restarting, or the network may be down.'
		});
	}

	if (response.status === 401 && !options.expectUnauthorised) onUnauthorised();

	if (response.status === 204 || response.status === 205) return undefined as T;

	const text = await response.text();
	let parsed: unknown = undefined;
	if (text) {
		try {
			parsed = JSON.parse(text);
		} catch {
			parsed = undefined;
		}
	}

	if (!response.ok) {
		// A problem document is the contract, but a reverse proxy in front of the
		// binary can answer a 502 in HTML and that must not surface as "undefined".
		const problem = parsed as Partial<Problem> | undefined;
		throw new ApiError({
			type: problem?.type ?? ERROR_BASE + 'unexpected',
			title: problem?.title ?? response.statusText ?? 'Request failed',
			status: problem?.status ?? response.status,
			detail: problem?.detail,
			instance: problem?.instance,
			errors: problem?.errors
		});
	}

	return parsed as T;
}

export const api = {
	get: <T>(path: string, options: RequestOptions = {}) => request<T>(path, { ...options }),
	post: <T>(path: string, body?: unknown, options: RequestOptions = {}) =>
		request<T>(path, { ...options, method: 'POST', body }),
	patch: <T>(path: string, body?: unknown, options: RequestOptions = {}) =>
		request<T>(path, { ...options, method: 'PATCH', body }),
	delete: <T>(path: string, options: RequestOptions = {}) =>
		request<T>(path, { ...options, method: 'DELETE' })
};
