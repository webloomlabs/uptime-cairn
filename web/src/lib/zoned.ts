/**
 * Wall-clock times in a named zone, for `datetime-local` inputs.
 *
 * A maintenance window carries its own IANA zone, and "02:00" on its form means
 * 02:00 there, not in the browser's zone. A `datetime-local` value has no zone
 * at all, and `new Date(value)` would read it as the browser's, so these convert
 * through `Intl` explicitly.
 */

const INPUT = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})$/;

/** The browser's own zone, which is the sensible default for a new window. */
export function browserZone(): string {
	try {
		return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
	} catch {
		return 'UTC';
	}
}

/** Every zone the browser knows, for a suggestion list. Empty when unsupported. */
export function knownZones(): string[] {
	const intl = Intl as typeof Intl & { supportedValuesOf?: (key: string) => string[] };
	try {
		return intl.supportedValuesOf?.('timeZone') ?? [];
	} catch {
		return [];
	}
}

/** Whether the browser accepts this zone name. The server has the final word. */
export function isZone(zone: string): boolean {
	try {
		new Intl.DateTimeFormat('en-US', { timeZone: zone });
		return true;
	} catch {
		return false;
	}
}

type Parts = { year: number; month: number; day: number; hour: number; minute: number };

function partsIn(at: number, zone: string): Parts {
	const format = new Intl.DateTimeFormat('en-US', {
		timeZone: zone,
		hourCycle: 'h23',
		year: 'numeric',
		month: '2-digit',
		day: '2-digit',
		hour: '2-digit',
		minute: '2-digit'
	});
	const parts: Record<string, string> = {};
	for (const part of format.formatToParts(new Date(at))) parts[part.type] = part.value;
	return {
		year: Number(parts.year),
		month: Number(parts.month),
		day: Number(parts.day),
		hour: Number(parts.hour),
		minute: Number(parts.minute)
	};
}

/** The zone's offset from UTC at an instant, in milliseconds, to the minute. */
function offsetAt(at: number, zone: string): number {
	const p = partsIn(at, zone);
	const wall = Date.UTC(p.year, p.month - 1, p.day, p.hour, p.minute);
	return wall - Math.floor(at / 60_000) * 60_000;
}

const pad = (n: number) => String(n).padStart(2, '0');

/** An instant as a `datetime-local` value in the zone. */
export function toZonedInput(iso: string | null | undefined, zone: string): string {
	if (!iso) return '';
	const at = Date.parse(iso);
	if (Number.isNaN(at)) return '';
	const z = isZone(zone) ? zone : 'UTC';
	const p = partsIn(at, z);
	return `${p.year}-${pad(p.month)}-${pad(p.day)}T${pad(p.hour)}:${pad(p.minute)}`;
}

/**
 * A `datetime-local` value read as wall-clock time in the zone, as RFC 3339 UTC.
 *
 * The offset is looked up twice because it depends on the instant being found:
 * the first guess can sit on the far side of a daylight-saving change. A time
 * the spring change skipped resolves forward, as the server's evaluator does.
 * An unknown zone reads as UTC, and the server then names the zone as invalid.
 */
export function fromZonedInput(value: string, zone: string): string | undefined {
	const match = INPUT.exec(value);
	if (!match) return undefined;
	const [, y, mo, d, h, mi] = match.map(Number);
	const z = isZone(zone) ? zone : 'UTC';

	const wall = Date.UTC(y, mo - 1, d, h, mi);
	let at = wall - offsetAt(wall, z);
	const corrected = wall - offsetAt(at, z);
	if (corrected !== at) at = corrected;
	return new Date(at).toISOString();
}

/** Now, as a `datetime-local` value in the zone. */
export function nowInZone(zone: string): string {
	return toZonedInput(new Date().toISOString(), zone);
}

/** Moves a `datetime-local` value by some minutes, keeping it in the zone. */
export function addMinutes(value: string, minutes: number, zone: string): string {
	const iso = fromZonedInput(value, zone);
	if (!iso) return value;
	return toZonedInput(new Date(Date.parse(iso) + minutes * 60_000).toISOString(), zone);
}
