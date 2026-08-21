import { getCSRFToken } from './api';
import type { MonitorStatus } from './types';

/**
 * The live-update channel, ADR-004's other half.
 *
 * The list controller was already written against "subscribe to visible IDs,
 * reconcile on interval" — the ADR is explicit that that has to be true from the
 * start, or upgrading the mechanism later is a rewrite rather than an addition.
 * This is the addition: the same controller, with a stream underneath it, so a
 * row that changes status updates now instead of on the next poll.
 *
 * Three properties are load-bearing and none of them is about latency.
 *
 * **Scope is the viewport.** The stream carries diffs for exactly the monitor
 * ids currently on screen. A monitor nobody is looking at produces no traffic
 * here at all, which is what makes the failure mode this ADR exists to avoid
 * structurally impossible rather than merely tuned around: there is no monitor
 * count at which an unwatched row starts costing this browser anything.
 *
 * **Scope changes in place.** Paginating sends the new ids to the stream's own
 * endpoint; the connection keeps running. A channel torn down and re-established
 * on every page turn would spend its life reconnecting.
 *
 * **Reconciliation does not go away.** The stream cannot tell a filtered view
 * that a monitor it is *not* watching has just started matching the filter —
 * nothing is subscribed to it. So the membership poll stays exactly as it was,
 * and the stream is what makes the rows already on screen near-instant. Removing
 * the poll because "we have push now" is the specific mistake the ADR's
 * Consequences section warns about.
 */

/** One monitor's diff, matching MonitorLiveUpdate in the OpenAPI spec. */
export type MonitorUpdate = {
	monitor_id: string;
	status: MonitorStatus;
	at: string;
	response_time_ms: number | null;
	important: boolean;
	message?: string;
	state_version: number;
};

/** The global header counts, matching MonitorSummary. */
export type MonitorSummary = {
	counts: Record<string, number>;
	at: string;
};

/**
 * How long to wait before reconnecting, and why it backs off.
 *
 * A server restarting during a deploy will refuse every connection for a few
 * seconds. A browser retrying every 200ms through that produces a burst of
 * failed requests in the log at the exact moment somebody is reading it to find
 * out what went wrong. Capped, because a dashboard left open overnight has to
 * come back on its own when the server does.
 */
const RECONNECT_MIN_MS = 1000;
const RECONNECT_MAX_MS = 30_000;

export class LiveUpdates {
	/** True while a stream is open and has been given its id. */
	connected = $state(false);

	/**
	 * True once the server has answered 501 — this build has no bus wired in.
	 * Distinct from "not connected", because the answer is different: a
	 * disconnected stream will retry, an unavailable one never will, and a
	 * dashboard that retried forever against a 501 would be a permanent stream of
	 * failed requests for a feature that is simply not there.
	 */
	unavailable = $state(false);

	private source: EventSource | null = null;
	private streamId: string | null = null;
	private scope: string[] = [];
	private backoff = RECONNECT_MIN_MS;
	private retry: ReturnType<typeof setTimeout> | null = null;
	private closed = false;

	constructor(
		private readonly onUpdate: (update: MonitorUpdate) => void,
		private readonly onSummary: (summary: MonitorSummary) => void
	) {}

	/** Opens the stream, subscribing to the ids given. */
	start(monitorIds: string[]): void {
		this.closed = false;
		this.scope = [...monitorIds];
		this.open();
	}

	/**
	 * Replaces what the stream carries.
	 *
	 * In place while the stream is open; remembered for the next connection when
	 * it is not, so a scope change during a reconnect is not lost — which is the
	 * common case, because paginating and losing the connection both happen when
	 * somebody has just come back to a tab.
	 */
	async setScope(monitorIds: string[]): Promise<void> {
		this.scope = [...monitorIds];
		if (!this.streamId) return;

		try {
			const response = await fetch(`/api/v1/live/${this.streamId}/scope`, {
				method: 'PUT',
				credentials: 'same-origin',
				headers: {
					'Content-Type': 'application/json',
					...(getCSRFToken() ? { 'X-Cairn-CSRF-Token': getCSRFToken() as string } : {})
				},
				body: JSON.stringify({ monitor_ids: this.scope })
			});
			// A stream the server no longer knows about: reconnect rather than
			// carrying on against an id that will never deliver anything again.
			if (response.status === 404) this.reopen();
		} catch {
			// The stream itself will notice and reconnect. Retrying the scope
			// change here would race with that.
		}
	}

	/** Closes the stream and stops reconnecting. */
	stop(): void {
		this.closed = true;
		if (this.retry !== null) clearTimeout(this.retry);
		this.retry = null;
		this.source?.close();
		this.source = null;
		this.streamId = null;
		this.connected = false;
	}

	private open(): void {
		if (this.closed || this.unavailable) return;
		this.source?.close();

		// The scope rides the opening request, so the first page of rows starts
		// updating without a second round trip.
		const query = this.scope.length ? `?monitor_ids=${this.scope.join(',')}` : '';
		const source = new EventSource(`/api/v1/live${query}`, { withCredentials: true });
		this.source = source;

		source.addEventListener('stream', (event) => {
			try {
				this.streamId = (
					JSON.parse((event as MessageEvent).data) as { stream_id: string }
				).stream_id;
				this.connected = true;
				this.backoff = RECONNECT_MIN_MS;
			} catch {
				// A frame we cannot parse is a frame from something that is not this
				// server. Left disconnected, which reconnects.
			}
		});

		source.addEventListener('monitor', (event) => {
			try {
				this.onUpdate(JSON.parse((event as MessageEvent).data) as MonitorUpdate);
			} catch {
				// One malformed frame must not take the stream down.
			}
		});

		source.addEventListener('summary', (event) => {
			try {
				this.onSummary(JSON.parse((event as MessageEvent).data) as MonitorSummary);
			} catch {
				// As above.
			}
		});

		source.onerror = () => {
			this.connected = false;
			source.close();
			if (this.closed) return;

			// EventSource reports every failure identically, including the 501 that
			// means this build has no bus. Asking once, cheaply, is what separates
			// "retry" from "stop retrying forever".
			void this.probe();
		};
	}

	/**
	 * Finds out whether the stream is worth retrying.
	 *
	 * EventSource gives no status code on failure, so a build without a live bus
	 * is indistinguishable from a server that is restarting — and retrying
	 * forever against the first would be a permanent stream of failed requests
	 * for a feature that is not there. One HEAD-shaped request answers it.
	 */
	private async probe(): Promise<void> {
		try {
			const response = await fetch('/api/v1/live?monitor_ids=', {
				method: 'GET',
				credentials: 'same-origin',
				headers: { Accept: 'text/event-stream' }
			});
			// Read nothing and hang up immediately: this is a status check, not a
			// second stream.
			void response.body?.cancel();

			if (response.status === 501) {
				this.unavailable = true;
				return;
			}
			if (response.status === 401 || response.status === 403) {
				// The session is gone. The API client's own 401 handling will move
				// the app to the login screen; retrying here would just add noise.
				this.unavailable = true;
				return;
			}
		} catch {
			// Unreachable. That is exactly the case worth retrying.
		}
		this.scheduleReopen();
	}

	private reopen(): void {
		this.source?.close();
		this.streamId = null;
		this.open();
	}

	private scheduleReopen(): void {
		if (this.closed || this.retry !== null) return;
		const delay = this.backoff;
		this.backoff = Math.min(this.backoff * 2, RECONNECT_MAX_MS);
		this.retry = setTimeout(() => {
			this.retry = null;
			this.open();
		}, delay);
	}
}
