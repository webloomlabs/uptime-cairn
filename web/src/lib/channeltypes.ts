import type { FieldSpec } from './monitortypes';

/**
 * What each notification channel asks for.
 *
 * Mirrors `schemas` in internal/notify/config.go, which is the authority: the
 * server validates against that table and answers with a JSON pointer per bad
 * field, so a mistake here surfaces as a highlighted control rather than as a
 * silently wrong channel.
 *
 * `secret: true` fields come back from a read as the redaction marker and are
 * submitted unchanged, which is what lets a channel be renamed without its
 * credential being retyped. `template: true` marks the fields the live preview
 * can render.
 */

export type ChannelFieldSpec = FieldSpec & { secret?: boolean; template?: boolean };

export type ChannelSpec = {
	label: string;
	summary: string;
	fields: ChannelFieldSpec[];
};

const messageTemplate: ChannelFieldSpec = {
	key: 'message_template',
	label: 'Message template',
	kind: 'text',
	template: true,
	hint: 'Leave empty for the default message.'
};

export const CHANNEL_TYPES: Record<string, ChannelSpec> = {
	email: {
		label: 'Email (SMTP)',
		summary: 'Send mail, through the instance relay or a relay of this channel’s own.',
		fields: [
			{ key: 'to', label: 'To', kind: 'list', required: true, hint: 'One address per line.' },
			{ key: 'cc', label: 'Cc', kind: 'list' },
			{
				key: 'use_instance_smtp',
				label: 'Use the instance SMTP settings',
				kind: 'boolean',
				hint: 'Turn this off to give the channel its own relay.'
			},
			{ key: 'smtp_host', label: 'SMTP host', kind: 'text' },
			{ key: 'smtp_port', label: 'SMTP port', kind: 'number', min: 1, max: 65535 },
			{ key: 'smtp_username', label: 'SMTP username', kind: 'text' },
			{ key: 'smtp_password', label: 'SMTP password', kind: 'secret', secret: true },
			{
				key: 'smtp_encryption',
				label: 'Encryption',
				kind: 'select',
				options: ['', 'none', 'starttls', 'tls']
			},
			{ key: 'from_address', label: 'From address', kind: 'text' },
			{ key: 'from_name', label: 'From name', kind: 'text' }
		]
	},
	webhook: {
		label: 'Webhook',
		summary: 'POST your own body to your own URL, with every variable interpolated.',
		fields: [
			{ key: 'url', label: 'URL', kind: 'url', required: true },
			{
				key: 'method',
				label: 'Method',
				kind: 'select',
				options: ['POST', 'PUT', 'PATCH', 'GET']
			},
			{ key: 'content_type', label: 'Content type', kind: 'text', advanced: true },
			{
				key: 'body_template',
				label: 'Body template',
				kind: 'text',
				template: true,
				hint: 'Leave empty for the default event envelope.'
			},
			{ key: 'verify_tls', label: 'Verify the TLS certificate', kind: 'boolean', advanced: true },
			{ key: 'timeout_seconds', label: 'Timeout', kind: 'number', min: 1, max: 60, advanced: true }
		]
	},
	slack: {
		label: 'Slack',
		summary: 'Post to an incoming webhook.',
		fields: [
			{ key: 'webhook_url', label: 'Webhook URL', kind: 'secret', required: true, secret: true },
			{ key: 'channel', label: 'Channel', kind: 'text' },
			{ key: 'username', label: 'Username', kind: 'text', advanced: true },
			{ key: 'icon_emoji', label: 'Icon emoji', kind: 'text', advanced: true },
			messageTemplate
		]
	},
	discord: {
		label: 'Discord',
		summary: 'Post to a channel webhook.',
		fields: [
			{ key: 'webhook_url', label: 'Webhook URL', kind: 'secret', required: true, secret: true },
			{ key: 'username', label: 'Username', kind: 'text', advanced: true },
			{ key: 'avatar_url', label: 'Avatar URL', kind: 'url', advanced: true },
			messageTemplate
		]
	},
	telegram: {
		label: 'Telegram',
		summary: 'Send through a bot.',
		fields: [
			{ key: 'bot_token', label: 'Bot token', kind: 'secret', required: true, secret: true },
			{ key: 'chat_id', label: 'Chat ID', kind: 'text', required: true },
			{ key: 'message_thread_id', label: 'Thread ID', kind: 'text', advanced: true },
			{
				key: 'parse_mode',
				label: 'Parse mode',
				kind: 'select',
				options: ['', 'none', 'Markdown', 'MarkdownV2', 'HTML'],
				advanced: true
			},
			{ key: 'disable_notification', label: 'Send silently', kind: 'boolean', advanced: true },
			messageTemplate
		]
	},
	matrix: {
		label: 'Matrix',
		summary: 'Send to a room. Retries reuse the event id, so a timeout cannot double-post.',
		fields: [
			{ key: 'homeserver_url', label: 'Homeserver URL', kind: 'url', required: true },
			{ key: 'room_id', label: 'Room ID', kind: 'text', required: true },
			{ key: 'access_token', label: 'Access token', kind: 'secret', required: true, secret: true },
			messageTemplate
		]
	},
	gotify: {
		label: 'Gotify',
		summary: 'Push to a self-hosted Gotify server.',
		fields: [
			{ key: 'server_url', label: 'Server URL', kind: 'url', required: true },
			{
				key: 'application_token',
				label: 'Application token',
				kind: 'secret',
				required: true,
				secret: true
			},
			{ key: 'priority', label: 'Priority', kind: 'number', min: 0, max: 10 },
			messageTemplate
		]
	},
	ntfy: {
		label: 'ntfy',
		summary: 'Publish to a topic.',
		fields: [
			{ key: 'topic', label: 'Topic', kind: 'text', required: true },
			{ key: 'server_url', label: 'Server URL', kind: 'url', hint: 'Defaults to ntfy.sh.' },
			{ key: 'priority', label: 'Priority', kind: 'number', min: 1, max: 5 },
			{ key: 'tags', label: 'Tags', kind: 'list' },
			{
				key: 'auth_type',
				label: 'Authentication',
				kind: 'select',
				options: ['', 'none', 'basic', 'token'],
				advanced: true
			},
			{ key: 'username', label: 'Username', kind: 'text', advanced: true },
			{ key: 'password', label: 'Password', kind: 'secret', secret: true, advanced: true },
			{ key: 'token', label: 'Token', kind: 'secret', secret: true, advanced: true },
			messageTemplate
		]
	},
	msteams: {
		label: 'Microsoft Teams',
		summary: 'Post to an incoming webhook.',
		fields: [
			{ key: 'webhook_url', label: 'Webhook URL', kind: 'secret', required: true, secret: true },
			messageTemplate
		]
	},
	pagerduty: {
		label: 'PagerDuty',
		summary: 'Open and resolve incidents. A recovery closes the alert the failure opened.',
		fields: [
			{
				key: 'integration_key',
				label: 'Integration key',
				kind: 'secret',
				required: true,
				secret: true
			},
			{
				key: 'severity',
				label: 'Severity',
				kind: 'select',
				options: ['', 'critical', 'error', 'warning', 'info']
			},
			{ key: 'region', label: 'Region', kind: 'select', options: ['', 'us', 'eu'] },
			{ key: 'auto_resolve', label: 'Resolve on recovery', kind: 'boolean' }
		]
	},
	opsgenie: {
		label: 'Opsgenie',
		summary: 'Create and close alerts.',
		fields: [
			{ key: 'api_key', label: 'API key', kind: 'secret', required: true, secret: true },
			{ key: 'region', label: 'Region', kind: 'select', options: ['', 'us', 'eu'] },
			{
				key: 'priority',
				label: 'Priority',
				kind: 'select',
				options: ['', 'P1', 'P2', 'P3', 'P4', 'P5']
			},
			{ key: 'auto_close', label: 'Close on recovery', kind: 'boolean' }
		]
	},
	twilio: {
		label: 'Twilio / SMS',
		summary: 'Send a text message.',
		fields: [
			{ key: 'account_sid', label: 'Account SID', kind: 'text', required: true },
			{ key: 'auth_token', label: 'Auth token', kind: 'secret', required: true, secret: true },
			{ key: 'from_number', label: 'From number', kind: 'text', required: true },
			{
				key: 'to_numbers',
				label: 'To numbers',
				kind: 'list',
				required: true,
				hint: 'One per line, in E.164 form.'
			},
			messageTemplate
		]
	},
	apprise: {
		label: 'Apprise',
		summary:
			'Hand off to a locally installed Apprise. Each URL embeds its own credentials, so they are stored as secrets.',
		fields: [
			{
				key: 'urls',
				label: 'Apprise URLs',
				kind: 'list',
				required: true,
				secret: true,
				hint: 'One per line.'
			},
			{ key: 'title_template', label: 'Title template', kind: 'text', template: true },
			{ key: 'body_template', label: 'Body template', kind: 'text', template: true }
		]
	}
};

export function channelSpec(type: string): ChannelSpec | null {
	return CHANNEL_TYPES[type] ?? null;
}
