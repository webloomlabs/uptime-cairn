package auth

import "strings"

// Scope is a permission grant, spelled <resource>:<read|write> exactly as the
// OpenAPI spec fixes it. Every operation declares what it needs in
// x-cairn-scopes, and this package is where that declaration is enforced.
type Scope string

// The scope set, matching ApiKeyScope in docs/api/openapi.yaml. Scopes for
// unbuilt features are listed too: a key created today naming a Phase 2 scope
// stays valid when that feature ships, and rejecting it now would make every
// such key a migration later.
const (
	ScopeMonitorsRead      Scope = "monitors:read"
	ScopeMonitorsWrite     Scope = "monitors:write"
	ScopeHeartbeatsRead    Scope = "heartbeats:read"
	ScopeGroupsRead        Scope = "groups:read"
	ScopeGroupsWrite       Scope = "groups:write"
	ScopeTagsRead          Scope = "tags:read"
	ScopeTagsWrite         Scope = "tags:write"
	ScopeNotificationsRead Scope = "notifications:read"
	ScopeNotificationsWrit Scope = "notifications:write"
	ScopeMaintenanceRead   Scope = "maintenance:read"
	ScopeMaintenanceWrite  Scope = "maintenance:write"
	ScopeIncidentsRead     Scope = "incidents:read"
	ScopeIncidentsWrite    Scope = "incidents:write"
	ScopeStatusPagesRead   Scope = "status_pages:read"
	ScopeStatusPagesWrite  Scope = "status_pages:write"
	ScopeWebhooksRead      Scope = "webhooks:read"
	ScopeWebhooksWrite     Scope = "webhooks:write"
	ScopeImportsWrite      Scope = "imports:write"
	ScopeSettingsRead      Scope = "settings:read"
	ScopeSettingsWrite     Scope = "settings:write"
	ScopeUsersRead         Scope = "users:read"
	ScopeUsersWrite        Scope = "users:write"
	ScopeAPIKeysRead       Scope = "api_keys:read"
	ScopeAPIKeysWrite      Scope = "api_keys:write"
	ScopeMetricsRead       Scope = "metrics:read"
	ScopeTeamsRead         Scope = "teams:read"
	ScopeTeamsWrite        Scope = "teams:write"
	ScopeSchedulesRead     Scope = "schedules:read"
	ScopeSchedulesWrite    Scope = "schedules:write"
	ScopeReportsRead       Scope = "reports:read"
	ScopeReportsWrite      Scope = "reports:write"

	// Brand profiles are their own resource rather than part of reports:*, which
	// is the spec's choice and the right one: a branded template is edited by
	// whoever writes reports, but a client's logo and colours are an account
	// asset an agency may not want every report author replacing.
	ScopeBrandProfilesRead  Scope = "brand_profiles:read"
	ScopeBrandProfilesWrite Scope = "brand_profiles:write"
)

// AllScopes is every scope the spec defines, which is also what the owner role
// holds.
var AllScopes = []Scope{
	ScopeMonitorsRead, ScopeMonitorsWrite, ScopeHeartbeatsRead,
	ScopeGroupsRead, ScopeGroupsWrite, ScopeTagsRead, ScopeTagsWrite,
	ScopeNotificationsRead, ScopeNotificationsWrit,
	ScopeMaintenanceRead, ScopeMaintenanceWrite,
	ScopeIncidentsRead, ScopeIncidentsWrite,
	ScopeStatusPagesRead, ScopeStatusPagesWrite,
	ScopeWebhooksRead, ScopeWebhooksWrite, ScopeImportsWrite,
	ScopeSettingsRead, ScopeSettingsWrite,
	ScopeUsersRead, ScopeUsersWrite,
	ScopeAPIKeysRead, ScopeAPIKeysWrite, ScopeMetricsRead,
	ScopeTeamsRead, ScopeTeamsWrite,
	ScopeSchedulesRead, ScopeSchedulesWrite,
	ScopeReportsRead, ScopeReportsWrite,
	ScopeBrandProfilesRead, ScopeBrandProfilesWrite,
}

// Valid reports whether s is a scope the spec defines.
func (s Scope) Valid() bool {
	for _, known := range AllScopes {
		if s == known {
			return true
		}
	}
	return false
}

// Implies reports whether holding s satisfies a requirement for want.
//
// write implies read on the same resource, and nothing else implies anything:
// a hierarchy where one scope quietly grants another is a hierarchy nobody can
// audit.
func (s Scope) Implies(want Scope) bool {
	if s == want {
		return true
	}
	resource, action, ok := strings.Cut(string(s), ":")
	if !ok || action != "write" {
		return false
	}
	return want == Scope(resource+":read")
}

// Set is a held set of scopes.
type Set []Scope

// Grants reports whether the set satisfies a required scope.
func (set Set) Grants(want Scope) bool {
	for _, held := range set {
		if held.Implies(want) {
			return true
		}
	}
	return false
}

// Covers reports whether the set holds everything in other. This is what stops
// a key from minting a more powerful key than its creator holds.
func (set Set) Covers(other Set) bool {
	for _, want := range other {
		if !set.Grants(want) {
			return false
		}
	}
	return true
}

// Strings renders the set for JSON and for storage.
func (set Set) Strings() []string {
	out := make([]string, len(set))
	for i, s := range set {
		out[i] = string(s)
	}
	return out
}

// ScopesFor returns what a role holds. Phase 1 only ever creates an owner; the
// rest are here because Phase 3's RBAC should be a table of grants rather than
// a rewrite of the authorisation path.
func ScopesFor(role string) Set {
	switch role {
	case "owner", "admin":
		return AllScopes
	case "editor":
		return Set{
			ScopeMonitorsRead, ScopeMonitorsWrite, ScopeHeartbeatsRead,
			ScopeGroupsRead, ScopeGroupsWrite, ScopeTagsRead, ScopeTagsWrite,
			ScopeNotificationsRead, ScopeNotificationsWrit,
			ScopeMaintenanceRead, ScopeMaintenanceWrite,
			ScopeIncidentsRead, ScopeIncidentsWrite,
			ScopeStatusPagesRead, ScopeStatusPagesWrite,
		}
	case "responder":
		return Set{
			ScopeMonitorsRead, ScopeHeartbeatsRead, ScopeGroupsRead, ScopeTagsRead,
			ScopeIncidentsRead, ScopeIncidentsWrite,
			ScopeMaintenanceRead, ScopeMaintenanceWrite,
		}
	case "viewer":
		return Set{
			ScopeMonitorsRead, ScopeHeartbeatsRead, ScopeGroupsRead, ScopeTagsRead,
			ScopeIncidentsRead, ScopeStatusPagesRead, ScopeMaintenanceRead,
		}
	default:
		return nil
	}
}
