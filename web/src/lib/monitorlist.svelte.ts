import { api } from './api';
import { LiveUpdates, type MonitorSummary, type MonitorUpdate } from './live.svelte';
import type { MembershipSignal, Monitor, Page } from './types';

/**
 * The monitor list, built to ADR-004.
 *
 * The ADR's model, and the reason this is a controller rather than a `fetch` in
 * a component:
 *
 *   - **The client is never sent full state.** Every load is a cursor-paginated,
 *     server-filtered, server-searched query. There is no "small install"
 *     shortcut that fetches everything when the count happens to be low today —
 *     the shortcut is the bug, because nothing tells you the day it stops being
 *     small.
 *   - **Freshness is bounded by reconciliation, not by push.** A filtered view
 *     cannot be kept live by subscribing to what is on screen: a monitor that
 *     goes down off-screen would now match `status=down` and nothing is
 *     subscribed to it. So `/monitors/membership` is polled — a version and a
 *     count, scoped to the active filter — and a change to the version raises
 *     `stale` rather than silently reordering rows under the pointer.
 *   - **Cost is bounded by the viewport.** Refreshing re-reads the pages
 *     currently held, not the collection. That is ADR-004's second load-test
 *     invariant, and it is the one a frontend can break on its own.
 *
 *   - **Live diffs are scoped to the viewport.** A stream carries changes for
 *     exactly the ids on screen, and paginating changes that scope in place
 *     rather than reopening the connection. It does not replace the poll and
 *     could not: nothing is subscribed to a monitor that is off-screen, so
 *     nothing can tell this view that one has just started matching its filter.
 *     Push makes the rows already held near-instant; reconciliation is what
 *     notices the set changing.
 *
 * The last point is the one worth guarding. Deleting the poll because "we have
 * push now" is the specific mistake the ADR's Consequences section warns about,
 * and the symptom is a "status = down" view that never shows the next incident.
 */

export const RECONCILE_INTERVAL_MS = 5000;

/**
 * How many recent checks each row's strip shows.
 *
 * Small on purpose. The server resolves the whole page's strips in one
 * statement, so the cost of this number is bytes on the wire rather than
 * queries — but bytes on the wire across a hundred rows is exactly the payload
 * ADR-004's second invariant bounds, and thirty ticks is already more history
 * than a row four pixels wide can distinguish.
 */
export const STRIP_LENGTH = 30;

export type MonitorFilters = {
	search: string;
	status: string[];
	type: string[];
	enabled: boolean | null;
	groupId: string | null;
	tagId: string | null;
};

export const emptyFilters: MonitorFilters = {
	search: '',
	status: [],
	type: [],
	enabled: null,
	groupId: null,
	tagId: null
};

/** The one place filters become query parameters, so the two cannot drift. */
function toQuery(filters: MonitorFilters): URLSearchParams {
	const params = new URLSearchParams();
	if (filters.search) params.set('search', filters.search);
	for (const status of filters.status) params.append('status', status);
	for (const type of filters.type) params.append('type', type);
	if (filters.enabled !== null) params.set('enabled', String(filters.enabled));
	if (filters.groupId) params.set('group_id', filters.groupId);
	if (filters.tagId) params.set('tag_id', filters.tagId);
	return params;
}

export class MonitorList {
	monitors = $state<Monitor[]>([]);
	loading = $state(false);
	loadingMore = $state(false);
	error = $state<unknown>(null);
	hasMore = $state(false);
	/** Total matching the current filter, from the membership signal. */
	total = $state<number | null>(null);
	/** Set when the server's membership version moves under a loaded view. */
	stale = $state(false);

	filters = $state<MonitorFilters>({ ...emptyFilters });
	readonly pageSize: number;

	/**
	 * The global status counts, pushed down the live channel.
	 *
	 * Server-computed and independent of this view's scope, which is the point:
	 * a header summed from whatever this page happens to be holding would report
	 * "3 down" for a filtered view containing three of the down monitors.
	 */
	counts = $state<Record<string, number> | null>(null);

	/** True while the live channel is delivering. */
	live = $state(false);

	private cursor: string | null = null;
	private version: number | null = null;
	private timer: ReturnType<typeof setInterval> | null = null;
	private updates: LiveUpdates | null = null;
	/** Guards against an in-flight page landing after the filters moved on. */
	private generation = 0;

	/**
	 * `initial` seeds the filter at construction rather than leaving a caller to
	 * assign `list.filters` afterwards. That is not a convenience: assigning it
	 * from inside an `$effect` reads and writes the same rune in one pass, which
	 * Svelte treats as a self-invalidating effect and aborts with
	 * `effect_update_depth_exceeded` — and an error thrown during an effect flush
	 * wedges the router, so every later navigation changes the URL and renders
	 * nothing.
	 */
	constructor(pageSize = 50, initial: Partial<MonitorFilters> = {}) {
		this.pageSize = pageSize;
		this.filters = { ...emptyFilters, ...initial };
	}

	private query(extra: Record<string, string> = {}): Record<string, string> {
		const params = toQuery(this.filters);
		for (const [key, value] of Object.entries(extra)) params.set(key, value);
		return Object.fromEntries(params);
	}

	/**
	 * A repeated key — two `status` values — cannot survive Object.fromEntries,
	 * so multi-valued filters are appended to the path directly. Collapsing
	 * `status=up&status=down` to one value would quietly return the wrong set.
	 */
	private path(extra: Record<string, string> = {}): string {
		const params = toQuery(this.filters);
		params.set('include', 'last_heartbeat,heartbeats,uptime');
		params.set('heartbeats_limit', String(STRIP_LENGTH));
		params.set('limit', String(this.pageSize));
		for (const [key, value] of Object.entries(extra)) params.set(key, value);
		const query = params.toString();
		return query ? `/monitors?${query}` : '/monitors';
	}

	async load(): Promise<void> {
		const generation = ++this.generation;
		this.loading = true;
		this.error = null;
		this.stale = false;
		this.cursor = null;

		try {
			const result = await api.get<Page<Monitor>>(this.path());
			if (generation !== this.generation) return;
			this.monitors = result.data;
			this.cursor = result.pagination.next_cursor;
			this.hasMore = result.pagination.has_more;
			this.resubscribe();
			await this.reconcile({ adopt: true });
		} catch (error) {
			if (generation === this.generation) this.error = error;
		} finally {
			if (generation === this.generation) this.loading = false;
		}
	}

	async loadMore(): Promise<void> {
		if (!this.hasMore || !this.cursor || this.loadingMore) return;
		const generation = this.generation;
		this.loadingMore = true;
		try {
			const result = await api.get<Page<Monitor>>(this.path({ cursor: this.cursor }));
			if (generation !== this.generation) return;
			// Cursor pagination on (updated_at, id) can still repeat a row that was
			// written between two pages. De-duplicating here costs a set and keeps
			// keyed-each from throwing on a duplicate key.
			const seen = new Set(this.monitors.map((m) => m.id));
			this.monitors = [...this.monitors, ...result.data.filter((m) => !seen.has(m.id))];
			this.cursor = result.pagination.next_cursor;
			this.hasMore = result.pagination.has_more;
			this.resubscribe();
		} catch (error) {
			if (generation === this.generation) this.error = error;
		} finally {
			if (generation === this.generation) this.loadingMore = false;
		}
	}

	/**
	 * Re-reads exactly the rows currently held, and nothing beyond them.
	 *
	 * This is what keeps client cost bounded by viewport rather than by monitor
	 * count: a view showing 50 of 5,000 re-reads 50. It refreshes in place rather
	 * than replacing the array wholesale so a row does not lose focus or jump
	 * while somebody is reading it.
	 */
	async refreshVisible(): Promise<void> {
		if (this.monitors.length === 0) return;
		const generation = this.generation;
		const held = this.monitors.length;

		try {
			// One page big enough to cover what is held. The alternative — a request
			// per visible monitor — is the fan-out problem again, pointed the other
			// way.
			const params = toQuery(this.filters);
			params.set('include', 'last_heartbeat,heartbeats,uptime');
			params.set('heartbeats_limit', String(STRIP_LENGTH));
			params.set('limit', String(Math.min(held, 200)));
			const result = await api.get<Page<Monitor>>(`/monitors?${params}`);
			if (generation !== this.generation) return;

			const fresh = new Map(result.data.map((m) => [m.id, m]));
			this.monitors = this.monitors.map((existing) => fresh.get(existing.id) ?? existing);
			this.stale = false;
		} catch {
			// A failed refresh leaves the last good view on screen. The next tick
			// tries again, and the reconciliation banner still says it is stale.
		}
	}

	/**
	 * Polls the membership signal.
	 *
	 * `adopt` takes the version as the baseline without raising staleness, which
	 * is what a fresh load wants; every later tick compares against it.
	 */
	async reconcile({ adopt = false } = {}): Promise<void> {
		const generation = this.generation;
		try {
			const params = toQuery(this.filters);
			const query = params.toString();
			const signal = await api.get<MembershipSignal>(
				query ? `/monitors/membership?${query}` : '/monitors/membership'
			);
			if (generation !== this.generation) return;

			this.total = signal.count;
			if (adopt || this.version === null) {
				this.version = signal.version;
				return;
			}
			if (signal.version !== this.version) {
				this.version = signal.version;
				this.stale = true;
			}
		} catch {
			// The signal is an optimisation. Losing it means the view stops
			// noticing changes on its own, not that it breaks.
		}
	}

	/**
	 * Starts reconciliation, and refreshes the rows on screen on the same tick.
	 *
	 * The two are separate concerns on one timer deliberately: membership answers
	 * "has the set changed", the refresh answers "have these rows changed". A
	 * status flip within the current page is the common case and never moves the
	 * membership version, so polling only the signal would leave a monitor
	 * showing green through its own outage.
	 */
	start(): void {
		this.stop();
		this.timer = setInterval(() => {
			void this.reconcile();
			void this.refreshVisible();
		}, RECONCILE_INTERVAL_MS);

		this.updates = new LiveUpdates(
			(update) => this.applyUpdate(update),
			(summary) => this.applySummary(summary)
		);
		this.updates.start(this.monitors.map((m) => m.id));
	}

	stop(): void {
		if (this.timer !== null) clearInterval(this.timer);
		this.timer = null;
		this.updates?.stop();
		this.updates = null;
		this.live = false;
	}

	/**
	 * Applies one live diff to the row it names.
	 *
	 * A diff for a monitor this view is not holding is dropped rather than
	 * appended. The stream is scoped to what is on screen, so receiving one at
	 * all means the scope has not caught up with a page turn yet — and inserting
	 * a row the current filter may not match would put a monitor on screen that
	 * the server never said belonged there.
	 *
	 * `state_version` is what makes this safe alongside the refresh on the timer:
	 * a diff older than what the row already holds is ignored, which is the race
	 * between a page refetch and the stream that was running during it.
	 */
	private applyUpdate(update: MonitorUpdate): void {
		this.live = true;

		const index = this.monitors.findIndex((m) => m.id === update.monitor_id);
		if (index < 0) return;

		const existing = this.monitors[index];
		const beat = {
			monitor_id: update.monitor_id,
			time: update.at,
			status: update.status,
			response_time_ms: update.response_time_ms,
			message: update.message ?? null,
			code: null,
			attempt: 1,
			important: update.important,
			suppressed: false,
			suppression_reason: null,
			probe_id: null
		};

		const next = [...this.monitors];
		next[index] = {
			...existing,
			status: update.status,
			last_check_at: update.at,
			last_heartbeat: beat,
			// Prepended, not refetched: the strip is a run of the most recent
			// checks and this is the most recent one. Bounded to the same length
			// the server would return, or a tab left open for a day would grow an
			// unbounded array behind a strip that draws thirty of them.
			heartbeats: existing.heartbeats
				? [beat, ...existing.heartbeats].slice(0, STRIP_LENGTH)
				: existing.heartbeats
		};
		this.monitors = next;
	}

	private applySummary(summary: MonitorSummary): void {
		this.live = true;
		this.counts = summary.counts;
	}

	/**
	 * Points the live channel at the rows now on screen.
	 *
	 * Called after anything that changes which monitors are held — a filter
	 * change, a page turn, a reload. The stream stays open across all of them;
	 * only what it carries changes, which is the operation an id-scoped
	 * subscription model makes cheap and a per-view connection would not.
	 */
	private resubscribe(): void {
		void this.updates?.setScope(this.monitors.map((m) => m.id));
	}

	/** Applies a filter change: a new query is a new collection, so paging restarts. */
	async apply(update: Partial<MonitorFilters>): Promise<void> {
		this.filters = { ...this.filters, ...update };
		this.version = null;
		await this.load();
	}

	async reset(): Promise<void> {
		await this.apply({ ...emptyFilters });
	}

	get filtered(): boolean {
		const f = this.filters;
		return Boolean(
			f.search || f.status.length || f.type.length || f.enabled !== null || f.groupId || f.tagId
		);
	}
}
