import { t, i18n } from './i18n/index.svelte';

/**
 * Formatting shared by every view.
 *
 * The rule these all follow: null is not zero. A monitor with no uptime figure
 * yet, a check that measured no response time, a day a status page has no data
 * for — each renders as an absence, never as 0%, 0 ms, or a red stone. Drawing a
 * missing measurement as a bad one is the single most common way a monitoring UI
 * lies, and it is called out for status pages specifically in the API
 * (`uptime_ratio` is nullable "because rendering that as downtime is the single
 * most common way a status page lies").
 */

/** An absent value, rendered so it reads as absent. */
export const ABSENT = '—';

export function formatUptime(ratio: number | null | undefined): string {
	if (ratio === null || ratio === undefined) return ABSENT;
	// Two decimals below 100 and none at it: "100.00%" reads as a measurement
	// that happens to be perfect, and at a glance down a list that is noise.
	const percent = ratio * 100;
	if (percent >= 99.995) return '100%';
	return `${percent.toFixed(2)}%`;
}

export function formatResponseTime(ms: number | null | undefined): string {
	if (ms === null || ms === undefined) return ABSENT;
	if (ms < 1) return '<1 ms';
	if (ms < 1000) return `${Math.round(ms)} ms`;
	return `${(ms / 1000).toFixed(2)} s`;
}

/** A duration in seconds, as an operator writes it: 20s, 5m, 2h, 1d. */
export function formatDuration(seconds: number | null | undefined): string {
	if (seconds === null || seconds === undefined) return ABSENT;
	if (seconds < 60) return `${seconds}s`;
	if (seconds < 3600) {
		const minutes = seconds / 60;
		return Number.isInteger(minutes) ? `${minutes}m` : `${minutes.toFixed(1)}m`;
	}
	if (seconds < 86400) {
		const hours = seconds / 3600;
		return Number.isInteger(hours) ? `${hours}h` : `${hours.toFixed(1)}h`;
	}
	const days = seconds / 86400;
	return Number.isInteger(days) ? `${days}d` : `${days.toFixed(1)}d`;
}

/**
 * Relative time, in the coarsest unit that is still honest.
 *
 * Deliberately not Intl.RelativeTimeFormat: the unit choice below is the part
 * that matters and would have to be written anyway, and the catalogue keys keep
 * the strings translatable. It bottoms out at "just now" rather than counting
 * single seconds, because a list that reticks every second is a list nobody can
 * read.
 */
export function formatRelative(iso: string | null | undefined, now = Date.now()): string {
	if (!iso) return t('common.never');
	const then = Date.parse(iso);
	if (Number.isNaN(then)) return ABSENT;

	const deltaSeconds = Math.round((now - then) / 1000);
	const future = deltaSeconds < 0;
	const magnitude = Math.abs(deltaSeconds);

	if (magnitude < 5) return t('time.justNow');
	if (magnitude < 60) return t(future ? 'time.inSeconds' : 'time.secondsAgo', { n: magnitude });
	if (magnitude < 3600)
		return t(future ? 'time.inMinutes' : 'time.minutesAgo', { n: Math.floor(magnitude / 60) });
	if (magnitude < 86400)
		return t(future ? 'time.inHours' : 'time.hoursAgo', { n: Math.floor(magnitude / 3600) });
	return t(future ? 'time.inDays' : 'time.daysAgo', { n: Math.floor(magnitude / 86400) });
}

/** An absolute timestamp in the viewer's own zone, for a tooltip or a detail row. */
export function formatAbsolute(iso: string | null | undefined): string {
	if (!iso) return t('common.never');
	const at = new Date(iso);
	if (Number.isNaN(at.getTime())) return ABSENT;
	return new Intl.DateTimeFormat(i18n.locale, {
		dateStyle: 'medium',
		timeStyle: 'medium'
	}).format(at);
}

export function formatDate(iso: string | null | undefined): string {
	if (!iso) return ABSENT;
	// A bar entry's `date` is a plain YYYY-MM-DD with no zone. Parsing it as a
	// date-time would shift it a day backwards for anyone west of UTC, so the
	// parts are read directly rather than handed to Date.
	const [year, month, day] = iso.slice(0, 10).split('-').map(Number);
	if (!year || !month || !day) return ABSENT;
	return new Intl.DateTimeFormat(i18n.locale, { dateStyle: 'medium' }).format(
		new Date(year, month - 1, day)
	);
}

/** The label for a monitor status, translated. */
export function statusLabel(status: string | null | undefined): string {
	if (!status) return t('status.unknown');
	const key = `status.${status}`;
	const label = t(key);
	return label === key ? status : label;
}

/**
 * A monitor's target, pulled out of its type-specific config for a list row.
 *
 * The config is opaque JSON by design — the probe is the only side that parses
 * it (ADR-005) — so this reads the few keys the spec names for display and shows
 * nothing rather than guessing when it recognises none of them.
 */
export function monitorTarget(monitor: {
	type: string;
	config?: Record<string, unknown> | null;
}): string {
	const config = monitor.config ?? {};
	const text = (key: string): string | null => {
		const value = config[key];
		return typeof value === 'string' && value !== '' ? value : null;
	};

	switch (monitor.type) {
		case 'http':
			return text('url') ?? '';
		case 'tcp': {
			const host = text('hostname');
			const port = config['port'];
			if (host && typeof port === 'number') return `${host}:${port}`;
			return host ?? '';
		}
		case 'grpc':
			return text('address') ?? '';
		case 'icmp':
			return text('hostname') ?? '';
		case 'dns': {
			const host = text('hostname') ?? '';
			const record = text('record_type');
			return record ? `${host} ${record}` : host;
		}
		case 'tls_expiry': {
			const host = text('hostname') ?? '';
			const port = config['port'];
			return host && typeof port === 'number' && port !== 443 ? `${host}:${port}` : host;
		}
		case 'domain_expiry':
			return text('domain') ?? '';
		case 'docker':
			return text('container') ?? '';
		case 'push':
			// A push monitor has no target by definition: the target calls us.
			return '';
		default:
			// An unknown type is a newer server than this build. Show nothing
			// rather than guessing at a key that may mean something else.
			return text('url') ?? text('hostname') ?? '';
	}
}
