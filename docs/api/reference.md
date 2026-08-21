# API reference

Every operation in the API, its scope, and the phase it ships in.

**Generated from [openapi.yaml](openapi.yaml) by `tools/apidoc` — do not edit.** Run
`go run ./tools/apidoc` after changing the spec. The spec is the contract; this
page exists so that reading it does not require a YAML viewer, and it is
generated precisely so it cannot come to disagree with what it describes.

For the conventions the whole surface follows — authentication, pagination,
error shape, the `include=` parameter — read [README.md](README.md) first. This
page is the index, not the introduction.

## At a glance

| | Count |
|---|---|
| Operations | 128 |
| Phase 1 operations | 94 |
| Phase 2 operations | 14 |
| Phase 3 operations | 20 |

## Reading the tables

**Scope** is what a credential must hold. `write` implies `read` on the same
resource, and a key can never be granted a scope its creator does not hold.

**public** means the operation declares no security at all: the push ingest, the
status page read path, and the subscription links a status page mails out. Those
are unauthenticated by design — `curl <url>` with no flags has to work, or the
feature does not get used — and each carries its credential in the path.

**Phase** is when it ships. An operation marked for a later phase is in the
contract and answers `501` in this build, naming itself, rather than `404` —
because "not yet" and "no such thing" are different answers and a client
generator should see the first.

## System

Liveness, readiness, build metadata, and Prometheus metrics.

| Operation | Scope | Phase | |
|---|---|---|---|
| `GET /api/v1/live` | `monitors:read` | 1 | Live updates for the monitors on screen |
| `PUT /api/v1/live/{streamId}/scope` | `monitors:read` | 1 | Change what an open stream carries |
| `GET /api/v1/overview` | — | 1 | Dashboard summary counts |
| `GET /api/v1/probes` | `monitors:read` | 1 | List the probes this install can place a monitor on |
| `GET /api/v1/system/info` | — | 1 | Instance metadata |
| `GET /healthz` | **public** | 1 | Liveness probe |
| `GET /metrics` | — | 1 | Prometheus metrics |
| `GET /readyz` | **public** | 1 | Readiness probe |

## Setup

First-run bootstrap. Available only until an admin exists.

| Operation | Scope | Phase | |
|---|---|---|---|
| `POST /api/v1/setup` | **public** | 1 | Create the first administrator account |
| `GET /api/v1/setup/status` | **public** | 1 | Whether first-run setup is still required |

## Authentication

Session login, logout, and TOTP two-factor enrolment.

| Operation | Scope | Phase | |
|---|---|---|---|
| `POST /api/v1/auth/login` | **public** | 1 | Establish a browser session |
| `POST /api/v1/auth/logout` | — | 1 | Destroy the current session |
| `GET /api/v1/auth/session` | — | 1 | Describe the caller |
| `DELETE /api/v1/auth/totp` | — | 1 | Disable TOTP |
| `POST /api/v1/auth/totp` | — | 1 | Begin TOTP enrolment |
| `POST /api/v1/auth/totp/confirm` | — | 1 | Activate TOTP enrolment |

## API Keys

Scoped, expiring, revocable keys for automation.

| Operation | Scope | Phase | |
|---|---|---|---|
| `GET /api/v1/api-keys` | `api_keys:read` | 1 | List API keys |
| `POST /api/v1/api-keys` | `api_keys:write` | 1 | Create an API key |
| `DELETE /api/v1/api-keys/{apiKeyId}` | `api_keys:write` | 1 | Revoke an API key |
| `GET /api/v1/api-keys/{apiKeyId}` | `api_keys:read` | 1 | Retrieve an API key |
| `PATCH /api/v1/api-keys/{apiKeyId}` | `api_keys:write` | 1 | Update an API key |

## Monitors

The checks themselves — creation, configuration, and control.

| Operation | Scope | Phase | |
|---|---|---|---|
| `GET /api/v1/monitors` | `monitors:read` | 1 | List monitors |
| `POST /api/v1/monitors` | `monitors:write` | 1 | Create a monitor |
| `POST /api/v1/monitors/bulk` | `monitors:write` | 1 | Apply one operation to many monitors |
| `GET /api/v1/monitors/membership` | `monitors:read` | 1 | Membership signal for a filtered view |
| `DELETE /api/v1/monitors/{monitorId}` | `monitors:write` | 1 | Delete a monitor |
| `GET /api/v1/monitors/{monitorId}` | `monitors:read` | 1 | Retrieve a monitor |
| `PATCH /api/v1/monitors/{monitorId}` | `monitors:write` | 1 | Update a monitor |
| `POST /api/v1/monitors/{monitorId}/check` | `monitors:write` | 1 | Run a check now |
| `POST /api/v1/monitors/{monitorId}/pause` | `monitors:write` | 1 | Pause a monitor |
| `POST /api/v1/monitors/{monitorId}/resume` | `monitors:write` | 1 | Resume a paused monitor |

## Monitor History

Heartbeats, rolled-up history, and uptime summaries.

| Operation | Scope | Phase | |
|---|---|---|---|
| `GET /api/v1/monitors/{monitorId}/certificate` | `monitors:read` | 1 | TLS certificate detail |
| `GET /api/v1/monitors/{monitorId}/heartbeats` | `heartbeats:read` | 1 | List raw heartbeats |
| `GET /api/v1/monitors/{monitorId}/history` | `heartbeats:read` | 1 | Rolled-up history |
| `GET /api/v1/monitors/{monitorId}/uptime` | `heartbeats:read` | 1 | Uptime summary |

## Groups

Organisational grouping of monitors.

| Operation | Scope | Phase | |
|---|---|---|---|
| `GET /api/v1/groups` | `groups:read` | 1 | List monitor groups |
| `POST /api/v1/groups` | `groups:write` | 1 | Create a group |
| `DELETE /api/v1/groups/{groupId}` | `groups:write` | 1 | Delete a group |
| `GET /api/v1/groups/{groupId}` | `groups:read` | 1 | Retrieve a group |
| `PATCH /api/v1/groups/{groupId}` | `groups:write` | 1 | Update a group |

## Tags

Cross-cutting labels used for filtering, status pages, and reports.

| Operation | Scope | Phase | |
|---|---|---|---|
| `GET /api/v1/tags` | `tags:read` | 1 | List tags |
| `POST /api/v1/tags` | `tags:write` | 1 | Create a tag |
| `DELETE /api/v1/tags/{tagId}` | `tags:write` | 1 | Delete a tag |
| `GET /api/v1/tags/{tagId}` | `tags:read` | 1 | Retrieve a tag |
| `PATCH /api/v1/tags/{tagId}` | `tags:write` | 1 | Update a tag |

## Notification Channels

Alert destinations and webhook payload templating.

| Operation | Scope | Phase | |
|---|---|---|---|
| `GET /api/v1/notification-channels` | `notifications:read` | 1 | List notification channels |
| `POST /api/v1/notification-channels` | `notifications:write` | 1 | Create a notification channel |
| `POST /api/v1/notification-channels/preview` | `notifications:read` | 1 | Render a webhook template without sending it |
| `GET /api/v1/notification-channels/template-variables` | `notifications:read` | 1 | List available template variables |
| `DELETE /api/v1/notification-channels/{channelId}` | `notifications:write` | 1 | Delete a notification channel |
| `GET /api/v1/notification-channels/{channelId}` | `notifications:read` | 1 | Retrieve a notification channel |
| `PATCH /api/v1/notification-channels/{channelId}` | `notifications:write` | 1 | Update a notification channel |
| `POST /api/v1/notification-channels/{channelId}/test` | `notifications:write` | 1 | Send a test notification |

## Maintenance Windows

Scheduled suppression windows attached to monitors, groups, or tags.

| Operation | Scope | Phase | |
|---|---|---|---|
| `GET /api/v1/maintenance-windows` | `maintenance:read` | 1 | List maintenance windows |
| `POST /api/v1/maintenance-windows` | `maintenance:write` | 1 | Create a maintenance window |
| `DELETE /api/v1/maintenance-windows/{maintenanceWindowId}` | `maintenance:write` | 1 | Delete a maintenance window |
| `GET /api/v1/maintenance-windows/{maintenanceWindowId}` | `maintenance:read` | 1 | Retrieve a maintenance window |
| `PATCH /api/v1/maintenance-windows/{maintenanceWindowId}` | `maintenance:write` | 1 | Update a maintenance window |

## Incidents

Incident records and their public timeline.

| Operation | Scope | Phase | |
|---|---|---|---|
| `GET /api/v1/incidents` | `incidents:read` | 1 | List incidents |
| `POST /api/v1/incidents` | `incidents:write` | 1 | Open an incident |
| `DELETE /api/v1/incidents/{incidentId}` | `incidents:write` | 1 | Delete an incident |
| `GET /api/v1/incidents/{incidentId}` | `incidents:read` | 1 | Retrieve an incident |
| `PATCH /api/v1/incidents/{incidentId}` | `incidents:write` | 1 | Update an incident |
| `POST /api/v1/incidents/{incidentId}/acknowledge` | `incidents:write` | 3 | Acknowledge an incident |
| `GET /api/v1/incidents/{incidentId}/updates` | `incidents:read` | 1 | List timeline entries |
| `POST /api/v1/incidents/{incidentId}/updates` | `incidents:write` | 1 | Post a timeline entry |

## Status Pages

Public status page configuration and subscribers.

| Operation | Scope | Phase | |
|---|---|---|---|
| `GET /api/v1/status-pages` | `status_pages:read` | 1 | List status pages |
| `POST /api/v1/status-pages` | `status_pages:write` | 1 | Create a status page |
| `DELETE /api/v1/status-pages/{statusPageId}` | `status_pages:write` | 1 | Delete a status page |
| `GET /api/v1/status-pages/{statusPageId}` | `status_pages:read` | 1 | Retrieve a status page |
| `PATCH /api/v1/status-pages/{statusPageId}` | `status_pages:write` | 1 | Update a status page |
| `GET /api/v1/status-pages/{statusPageId}/subscribers` | `status_pages:read` | 1 | List subscribers |
| `DELETE /api/v1/status-pages/{statusPageId}/subscribers/{subscriberId}` | `status_pages:write` | 1 | Remove a subscriber |

## Public

Unauthenticated read and ingest paths.

| Operation | Scope | Phase | |
|---|---|---|---|
| `GET /api/v1/public/status-pages/{slug}` | **public** | 1 | Read a published status page |
| `POST /api/v1/public/status-pages/{slug}/authenticate` | **public** | 1 | Exchange a page password for a visitor token |
| `POST /api/v1/public/status-pages/{slug}/subscribers` | **public** | 1 | Subscribe to status page updates |
| `DELETE /api/v1/public/subscriptions/{token}` | **public** | 1 | Unsubscribe |
| `POST /api/v1/public/subscriptions/{token}` | **public** | 1 | Confirm a subscription |
| `GET /api/v1/push/{pushToken}` | **public** | 1 | Record a push heartbeat |
| `POST /api/v1/push/{pushToken}` | **public** | 1 | Record a push heartbeat with a body |

## Outbound Webhooks

Event subscriptions delivered for every state change.

| Operation | Scope | Phase | |
|---|---|---|---|
| `GET /api/v1/webhooks` | `webhooks:read` | 1 | List event subscriptions |
| `POST /api/v1/webhooks` | `webhooks:write` | 1 | Subscribe to events |
| `DELETE /api/v1/webhooks/{webhookId}` | `webhooks:write` | 1 | Delete a subscription |
| `GET /api/v1/webhooks/{webhookId}` | `webhooks:read` | 1 | Retrieve a subscription |
| `PATCH /api/v1/webhooks/{webhookId}` | `webhooks:write` | 1 | Update a subscription |
| `GET /api/v1/webhooks/{webhookId}/deliveries` | `webhooks:read` | 1 | List delivery attempts |
| `POST /api/v1/webhooks/{webhookId}/deliveries/{deliveryId}/redeliver` | `webhooks:write` | 1 | Replay a delivery |

## Import

Uptime Kuma migration, including multi-instance merge.

| Operation | Scope | Phase | |
|---|---|---|---|
| `POST /api/v1/imports/kuma` | `imports:write` | 1 | Import from one or more Uptime Kuma databases |
| `GET /api/v1/imports/{importJobId}` | `imports:write` | 1 | Retrieve an import job and its report |

## Settings

Instance-wide configuration.

| Operation | Scope | Phase | |
|---|---|---|---|
| `GET /api/v1/settings` | `settings:read` | 1 | Retrieve instance settings |
| `PATCH /api/v1/settings` | `settings:write` | 1 | Update instance settings |

## Users

User accounts and roles.

| Operation | Scope | Phase | |
|---|---|---|---|
| `GET /api/v1/users` | `users:read` | 1 | List users |
| `POST /api/v1/users` | `users:write` | 3 | Create a user |
| `GET /api/v1/users/me` | — | 1 | Retrieve the authenticated user |
| `PATCH /api/v1/users/me` | — | 1 | Update the authenticated user |
| `DELETE /api/v1/users/{userId}` | `users:write` | 3 | Delete a user |
| `GET /api/v1/users/{userId}` | `users:read` | 1 | Retrieve a user |
| `PATCH /api/v1/users/{userId}` | `users:write` | 3 | Update a user |

## Teams

Team membership and role assignment. Ships in Phase 3.

| Operation | Scope | Phase | |
|---|---|---|---|
| `GET /api/v1/teams` | `teams:read` | 3 | List teams |
| `POST /api/v1/teams` | `teams:write` | 3 | Create a team |
| `DELETE /api/v1/teams/{teamId}` | `teams:write` | 3 | Delete a team |
| `GET /api/v1/teams/{teamId}` | `teams:read` | 3 | Retrieve a team |
| `PATCH /api/v1/teams/{teamId}` | `teams:write` | 3 | Update a team |

## On-Call

Schedules, rotations, and escalation policies. Ships in Phase 3.

| Operation | Scope | Phase | |
|---|---|---|---|
| `GET /api/v1/escalation-policies` | `schedules:read` | 3 | List escalation policies |
| `POST /api/v1/escalation-policies` | `schedules:write` | 3 | Create an escalation policy |
| `DELETE /api/v1/escalation-policies/{escalationPolicyId}` | `schedules:write` | 3 | Delete an escalation policy |
| `GET /api/v1/escalation-policies/{escalationPolicyId}` | `schedules:read` | 3 | Retrieve an escalation policy |
| `PATCH /api/v1/escalation-policies/{escalationPolicyId}` | `schedules:write` | 3 | Update an escalation policy |
| `GET /api/v1/schedules` | `schedules:read` | 3 | List on-call schedules |
| `POST /api/v1/schedules` | `schedules:write` | 3 | Create an on-call schedule |
| `DELETE /api/v1/schedules/{scheduleId}` | `schedules:write` | 3 | Delete an on-call schedule |
| `GET /api/v1/schedules/{scheduleId}` | `schedules:read` | 3 | Retrieve an on-call schedule |
| `PATCH /api/v1/schedules/{scheduleId}` | `schedules:write` | 3 | Update an on-call schedule |
| `GET /api/v1/schedules/{scheduleId}/on-call` | `schedules:read` | 3 | Who is on call |

## Reports

Report templates, schedules, and generated runs. Ships in Phase 2.

| Operation | Scope | Phase | |
|---|---|---|---|
| `GET /api/v1/report-runs` | `reports:read` | 2 | List report runs |
| `GET /api/v1/report-runs/{reportRunId}` | `reports:read` | 2 | Retrieve a report run |
| `GET /api/v1/report-runs/{reportRunId}/download` | `reports:read` | 2 | Download a generated report |
| `GET /api/v1/report-schedules` | `reports:read` | 2 | List report schedules |
| `POST /api/v1/report-schedules` | `reports:write` | 2 | Create a report schedule |
| `DELETE /api/v1/report-schedules/{reportScheduleId}` | `reports:write` | 2 | Delete a report schedule |
| `GET /api/v1/report-schedules/{reportScheduleId}` | `reports:read` | 2 | Retrieve a report schedule |
| `PATCH /api/v1/report-schedules/{reportScheduleId}` | `reports:write` | 2 | Update a report schedule |
| `GET /api/v1/report-templates` | `reports:read` | 2 | List report templates |
| `POST /api/v1/report-templates` | `reports:write` | 2 | Create a report template |
| `DELETE /api/v1/report-templates/{reportTemplateId}` | `reports:write` | 2 | Delete a report template |
| `GET /api/v1/report-templates/{reportTemplateId}` | `reports:read` | 2 | Retrieve a report template |
| `PATCH /api/v1/report-templates/{reportTemplateId}` | `reports:write` | 2 | Update a report template |
| `POST /api/v1/report-templates/{reportTemplateId}/generate` | `reports:write` | 2 | Generate a report now |

