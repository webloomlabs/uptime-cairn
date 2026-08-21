/**
 * What each monitor type asks for.
 *
 * The config object is opaque to everything except the probe (ADR-005): the
 * control plane carries it as bytes and never parses it. That makes this table
 * the frontend's own copy of a contract it does not own, so two rules apply.
 *
 * First, the field names come from the checkers themselves
 * (internal/probe/check/*.go) and the frozen spec — nothing here is invented. A
 * key this table gets wrong is a monitor that validates and then checks the
 * wrong thing.
 *
 * Second, a type the server offers but this table does not describe still works:
 * the form falls back to editing the config as JSON rather than refusing. That
 * is what keeps a newer server usable from an older dashboard instead of
 * hiding a monitor type behind a build.
 */

export const REDACTED = '__redacted__';

export type FieldKind = 'text' | 'url' | 'number' | 'boolean' | 'select' | 'list' | 'secret';

export type FieldSpec = {
	key: string;
	label: string;
	kind: FieldKind;
	required?: boolean;
	placeholder?: string;
	hint?: string;
	options?: string[];
	min?: number;
	max?: number;
	/** Rendered under a disclosure rather than in the main body. */
	advanced?: boolean;
};

export type TypeSpec = {
	label: string;
	/** The short form for a list chip, where "TLS certificate expiry" does not fit. */
	chip: string;
	summary: string;
	fields: FieldSpec[];
};

const IP_FAMILY: FieldSpec = {
	key: 'ip_family',
	label: 'IP family',
	kind: 'select',
	options: ['', 'ipv4', 'ipv6'],
	advanced: true,
	hint: 'Leave unset to let the resolver choose.'
};

export const MONITOR_TYPES: Record<string, TypeSpec> = {
	http: {
		label: 'HTTP(S)',
		chip: 'HTTP',
		summary: 'Request a URL and assert on the status code, body, or response time.',
		fields: [
			{ key: 'url', label: 'URL', kind: 'url', required: true, placeholder: 'https://example.com' },
			{
				key: 'method',
				label: 'Method',
				kind: 'select',
				options: ['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS']
			},
			{
				key: 'accepted_status_codes',
				label: 'Accepted status codes',
				kind: 'list',
				placeholder: '200-299',
				hint: 'One per line. Ranges are allowed. Defaults to 200-299.'
			},
			{
				key: 'response_time_threshold_ms',
				label: 'Response-time threshold (ms)',
				kind: 'number',
				min: 1,
				hint: 'A slower answer counts as down.'
			},
			{ key: 'follow_redirects', label: 'Follow redirects', kind: 'boolean', advanced: true },
			{ key: 'max_redirects', label: 'Maximum redirects', kind: 'number', min: 0, advanced: true },
			{ key: 'verify_tls', label: 'Verify the TLS certificate', kind: 'boolean', advanced: true },
			{ key: 'body', label: 'Request body', kind: 'text', advanced: true },
			IP_FAMILY
		]
	},
	tcp: {
		label: 'TCP port',
		chip: 'TCP',
		summary: 'Open a TCP connection and confirm something is listening.',
		fields: [
			{ key: 'hostname', label: 'Hostname', kind: 'text', required: true },
			{ key: 'port', label: 'Port', kind: 'number', required: true, min: 1, max: 65535 },
			IP_FAMILY
		]
	},
	icmp: {
		label: 'Ping (ICMP)',
		chip: 'PING',
		summary: 'Ping a host. Falls back to TCP where raw sockets are unavailable.',
		fields: [
			{ key: 'hostname', label: 'Hostname', kind: 'text', required: true },
			{ key: 'packet_count', label: 'Packets', kind: 'number', min: 1, max: 10, advanced: true },
			{ key: 'packet_size', label: 'Packet size', kind: 'number', min: 1, advanced: true },
			{
				key: 'fallback_to_tcp',
				label: 'Fall back to TCP',
				kind: 'boolean',
				hint: 'Containers often forbid the sockets ICMP needs.'
			},
			{ key: 'fallback_tcp_port', label: 'Fallback TCP port', kind: 'number', min: 1, max: 65535 },
			IP_FAMILY
		]
	},
	dns: {
		label: 'DNS record',
		chip: 'DNS',
		summary: 'Resolve a name and optionally assert on what comes back.',
		fields: [
			{ key: 'hostname', label: 'Hostname', kind: 'text', required: true },
			{
				key: 'record_type',
				label: 'Record type',
				kind: 'select',
				required: true,
				options: ['A', 'AAAA', 'CNAME', 'MX', 'NS', 'TXT', 'SOA', 'SRV', 'CAA', 'PTR']
			},
			{ key: 'resolver', label: 'Resolver', kind: 'text', placeholder: '1.1.1.1', advanced: true },
			{
				key: 'resolver_port',
				label: 'Resolver port',
				kind: 'number',
				min: 1,
				max: 65535,
				advanced: true
			},
			{ key: 'expected_values', label: 'Expected values', kind: 'list', hint: 'One per line.' },
			{
				key: 'match_mode',
				label: 'Match mode',
				kind: 'select',
				options: ['', 'any', 'all', 'exact'],
				advanced: true
			}
		]
	},
	tls_expiry: {
		label: 'TLS certificate expiry',
		chip: 'TLS',
		summary: 'Watch a certificate and alert before it expires.',
		fields: [
			{ key: 'hostname', label: 'Hostname', kind: 'text', required: true },
			{ key: 'port', label: 'Port', kind: 'number', min: 1, max: 65535, hint: 'Defaults to 443.' },
			{
				key: 'days_remaining_threshold',
				label: 'Warn when days remaining falls below',
				kind: 'number',
				min: 0,
				hint: 'This is the line an alert is drawn at. Zero means only once it has actually expired.'
			},
			{ key: 'server_name', label: 'SNI server name', kind: 'text', advanced: true },
			{ key: 'verify_chain', label: 'Verify the chain', kind: 'boolean', advanced: true }
		]
	},
	domain_expiry: {
		label: 'Domain expiry',
		chip: 'DOMAIN',
		summary: 'Watch a domain registration through RDAP, falling back to WHOIS.',
		fields: [
			{ key: 'domain', label: 'Domain', kind: 'text', required: true, placeholder: 'example.com' },
			{
				key: 'days_remaining_threshold',
				label: 'Warn when days remaining falls below',
				kind: 'number',
				min: 0
			},
			{
				key: 'source',
				label: 'Source',
				kind: 'select',
				options: ['', 'rdap', 'whois'],
				advanced: true,
				hint: 'Leave unset to try RDAP first.'
			}
		]
	},
	grpc: {
		label: 'gRPC health',
		chip: 'GRPC',
		summary: 'Call the standard gRPC health-checking service.',
		fields: [
			{ key: 'address', label: 'Address', kind: 'text', required: true, placeholder: 'host:50051' },
			{ key: 'service_name', label: 'Service name', kind: 'text' },
			{ key: 'use_tls', label: 'Use TLS', kind: 'boolean' },
			{ key: 'verify_tls', label: 'Verify the TLS certificate', kind: 'boolean', advanced: true },
			{ key: 'accepted_statuses', label: 'Accepted statuses', kind: 'list', advanced: true }
		]
	},
	docker: {
		label: 'Docker container',
		chip: 'DOCKER',
		summary: 'Ask a Docker daemon whether a container is running.',
		fields: [
			{ key: 'container', label: 'Container', kind: 'text', required: true },
			{
				key: 'docker_host',
				label: 'Docker host',
				kind: 'text',
				placeholder: 'unix:///var/run/docker.sock'
			},
			{ key: 'require_healthy', label: 'Require a healthy healthcheck', kind: 'boolean' }
		]
	},
	push: {
		label: 'Push (dead-man’s switch)',
		chip: 'PUSH',
		summary:
			'Nothing is checked from here. A job calls the push URL on its own schedule, and silence is the failure.',
		fields: []
	}
};

export function specFor(type: string): TypeSpec | null {
	return MONITOR_TYPES[type] ?? null;
}

/** The list-row chip label, falling back to the raw type for an unknown one. */
export function typeChip(type: string): string {
	return MONITOR_TYPES[type]?.chip ?? type.replace(/_/g, ' ').toUpperCase();
}
