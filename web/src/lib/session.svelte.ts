import {
	api,
	setCSRFToken,
	restoreCSRFToken,
	getCSRFToken,
	handleUnauthorised,
	ApiError
} from './api';
import type { Session, SystemInfo, User } from './types';

/**
 * Who is signed in, and what this build can do.
 *
 * The two are loaded together and deliberately so: /system/info is what
 * progressive disclosure runs on. A dashboard that hardcodes its own feature
 * list ships controls the server cannot honour — a subscribe box on an install
 * with no relay, an Apprise channel on a host with no apprise binary — and the
 * server already answers that question honestly (internal/api/system.go).
 * `can()` is the only way the rest of the UI is allowed to ask.
 */
class SessionState {
	user = $state<User | null>(null);
	scopes = $state<string[]>([]);
	info = $state<SystemInfo | null>(null);
	setupRequired = $state(false);
	/**
	 * Set when a session is valid but cannot write, which happens when the cookie
	 * outlives the CSRF token that pairs with it — a browser that logged in under
	 * an older build, or one whose storage was cleared. There is no way back from
	 * this except a fresh login: the token is issued only at login, and even
	 * `POST /auth/logout` is a write that needs it.
	 */
	reauthRequired = $state(false);
	/** False until the first resolve completes, so routes can wait rather than flash. */
	ready = $state(false);

	get authenticated(): boolean {
		return this.user !== null;
	}

	/** A capability this build and this configuration actually has. */
	can(capability: string): boolean {
		return this.info?.capabilities?.[capability] === true;
	}

	/**
	 * A scope the current principal holds. A cookie session gets the full set for
	 * its role; an API key gets what it was minted with. Checking here hides a
	 * control rather than letting the user find out from a 403.
	 */
	allows(scope: string): boolean {
		return this.scopes.includes(scope);
	}

	supportsType(type: string): boolean {
		return this.info?.monitor_types?.includes(type) ?? false;
	}

	adopt(session: Session): void {
		this.user = session.user ?? null;
		this.scopes = session.scopes ?? [];

		// Only when the response actually carries one. `GET /auth/session` returns
		// `csrf_token: null` deliberately — it is issued once, at login — so a null
		// here means "not issued by this endpoint", never "revoke the one you
		// have". Treating the two the same wipes the restored token on every page
		// load and leaves an authenticated session that cannot write.
		//
		// Revocation has its own path: clear(), called on sign-out and on a 401.
		if (session.csrf_token) setCSRFToken(session.csrf_token);
	}

	/**
	 * Resolves the current session from the cookie the browser already holds.
	 *
	 * A 401 here is the ordinary case, not an error: it is what a signed-out
	 * visitor looks like. So it is expected explicitly, which also keeps the
	 * global session-expiry hook from firing during startup.
	 */
	async load(): Promise<void> {
		// The CSRF token cannot be re-fetched: the server issues it once, at login,
		// and `GET /auth/session` deliberately withholds it. A reload therefore has
		// to recover the one the login response gave, or the session below comes
		// back authenticated and every write 403s. See api.ts.
		restoreCSRFToken();

		try {
			const status = await api.get<{ setup_required: boolean }>('/setup/status', {
				expectUnauthorised: true
			});
			this.setupRequired = status.setup_required;
		} catch {
			// An unreachable server is reported by whatever the route renders; it
			// must not leave the app permanently un-ready.
			this.setupRequired = false;
		}

		if (this.setupRequired) {
			this.clear();
			this.ready = true;
			return;
		}

		try {
			const session = await api.get<Session>('/auth/session', { expectUnauthorised: true });
			this.adopt(session);
		} catch (error) {
			if (error instanceof ApiError && error.status === 401) this.clear();
		}

		// An authenticated session with no CSRF token can read everything and write
		// nothing. Left alone it looks like a working dashboard where every save
		// answers 403, so it is turned into the one thing that actually fixes it:
		// a sign-in prompt. The stale cookie is simply replaced by the next login.
		if (this.authenticated && !getCSRFToken()) {
			this.clear();
			this.reauthRequired = true;
			this.ready = true;
			return;
		}

		if (this.authenticated) await this.loadInfo();
		this.ready = true;
	}

	async loadInfo(): Promise<void> {
		try {
			this.info = await api.get<SystemInfo>('/system/info');
		} catch {
			// Capabilities unknown means everything optional stays hidden, which
			// is the safe direction: a control that is missing is recoverable by
			// reloading, a control that 501s is not.
			this.info = null;
		}
	}

	async signOut(): Promise<void> {
		try {
			await api.post('/auth/logout');
		} catch {
			// The cookie may already be gone. Either way the local state goes.
		}
		this.clear();
	}

	clear(): void {
		this.user = null;
		this.scopes = [];
		this.info = null;
		setCSRFToken(null);
	}
}

export const session = new SessionState();

/**
 * Wires the client's 401 handling to the session.
 *
 * Called once from the root layout. It exists so `api.ts` does not have to know
 * about routing and the session does not have to wrap every call: a session that
 * expires while a dashboard is open should return the user to the sign-in page
 * on the next request, not throw an unhandled error into a component.
 */
export function bindSessionExpiry(onExpired: () => void): void {
	handleUnauthorised(() => {
		if (!session.authenticated) return;
		session.clear();
		onExpired();
	});
}
