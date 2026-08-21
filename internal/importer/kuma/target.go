package kuma

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/notify"
	"github.com/webloomlabs/uptime-cairn/internal/probe/check"
	"github.com/webloomlabs/uptime-cairn/internal/secrets"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Store is what the importer needs from persistence.
//
// Declared here by the consumer, and deliberately the same methods the API
// handlers call. That is what "writes through the repository layer, never around
// it" means in practice: an import produces rows indistinguishable from rows
// created through the API, including the encryption — a Kuma database holds
// basic-auth passwords and bot tokens in plaintext, and they are sealed on the
// way in by the same vault every other credential goes through.
type Store interface {
	CreateMonitor(ctx context.Context, m model.Monitor) error
	CreateGroup(ctx context.Context, g model.Group) error
	CreateTag(ctx context.Context, t model.Tag) error
	CreateChannel(ctx context.Context, c model.NotificationChannel) error
	CreateStatusPage(ctx context.Context, p model.StatusPage) error

	SetMonitorTags(ctx context.Context, monitorID, orgID model.ID, tagIDs []model.ID) error
	SetMonitorChannels(ctx context.Context, monitorID, orgID model.ID, channelIDs []model.ID) error

	ListTags(ctx context.Context, after *store.Cursor, limit int, search string) ([]model.TagSummary, bool, error)
	ListGroups(ctx context.Context, after *store.Cursor, limit int, search string) ([]model.GroupSummary, bool, error)
	ListChannels(ctx context.Context, after *store.Cursor, limit int, filter store.ChannelFilter) ([]store.ChannelWithCount, bool, error)
	StatusPageBySlug(ctx context.Context, slug string) (model.StatusPage, error)

	// MonitorNames is how a name collision is recognised before it is a
	// duplicate row. Names only: the question needs neither a config nor a
	// sealed envelope, and at 5,000 monitors that difference is the pass.
	MonitorNames(ctx context.Context) (map[model.ID]string, error)

	WriteBatch(ctx context.Context, beats []model.Heartbeat) (int64, error)

	CreateImportJob(ctx context.Context, j model.ImportJob) error
	UpdateImportJob(ctx context.Context, j model.ImportJob) error
	AddImportEntries(ctx context.Context, entries []model.ImportEntry) error
	GetImportJob(ctx context.Context, id model.ID) (model.ImportJob, []model.ImportEntry, error)
}

// Target wraps a Store with the encryption and validation an API write performs.
//
// It exists so the CLI and the HTTP endpoint share one write path. Two paths
// would be two sets of validation rules, and the one that drifts is always the
// one nobody exercises — which, for an import, is the CLI a user runs once.
type Target struct {
	store    Store
	registry *check.Registry
	configs  *secrets.Vault
	channels *notify.Vault
	orgID    model.ID
}

// NewTarget builds the write path. The vaults are the same ones the API holds,
// constructed from the same keeper, so there is one key hierarchy rather than a
// second one for imported rows.
func NewTarget(s Store, registry *check.Registry, keeper *secrets.Keeper, orgID model.ID) *Target {
	return &Target{
		store:    s,
		registry: registry,
		configs:  secrets.NewVault(keeper, "monitors", "config"),
		channels: notify.NewVault(keeper),
		orgID:    orgID,
	}
}

// createMonitor validates a config against its checker and writes the monitor
// with its credentials sealed.
//
// Validation is the checker's own, which is the same validation the API runs:
// an imported monitor that the probe would refuse at assignment time has to fail
// here, where the report can name it, rather than being written and then sitting
// pending forever with an error only the log has seen.
func (t *Target) createMonitor(ctx context.Context, m model.Monitor, config map[string]any) error {
	encoded, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	m.Config = encoded

	// A push monitor has no checker and never will: it is a deadline evaluated
	// by the control plane against the clock, and it is never assigned to a
	// probe at all (ADR-005 decision 6). Looking one up would fail on the one
	// type whose absence from the registry is the design.
	if m.Type != model.TypePush {
		checker, ok := t.registry.Lookup(m.Type)
		if !ok {
			return fmt.Errorf("this build has no checker for type %q", m.Type)
		}
		if err := checker.Validate(encoded); err != nil {
			return err
		}
	}

	if fields := t.registry.SecretFields(m.Type); len(fields) > 0 {
		public, secret, err := model.SplitConfig(m.Config, fields)
		if err != nil {
			return err
		}
		sealed, err := t.configs.Seal(m.OrgID[:], m.ID[:], secret)
		if err != nil {
			return err
		}
		m.Config, m.ConfigSecrets = public, sealed
	}
	return t.store.CreateMonitor(ctx, m)
}

// createChannel validates a channel config and writes it with its secrets
// sealed.
func (t *Target) createChannel(ctx context.Context, c model.NotificationChannel, config map[string]any) error {
	if problems := notify.Validate(c.Type, config); len(problems) > 0 {
		return fmt.Errorf("%s: %s", problems[0].Pointer, problems[0].Message)
	}

	public, secret := notify.Split(c.Type, config)
	encoded, err := json.Marshal(public)
	if err != nil {
		return fmt.Errorf("encode channel config: %w", err)
	}
	c.Config = encoded

	sealed, err := t.channels.Seal(c.OrgID, c.ID, secret)
	if err != nil {
		return err
	}
	c.Secrets = sealed
	return t.store.CreateChannel(ctx, c)
}

// existingNames reads what is already here, so a collision can be recognised
// before it is a constraint violation.
//
// Read once, up front, rather than queried per entity: an import of a thousand
// monitors would otherwise be a thousand extra reads, and the set it is checking
// against only grows by what this import itself adds — which is tracked in
// memory alongside.
func (t *Target) existingNames(ctx context.Context) (tags, groups, channels, monitors map[string]model.ID, err error) {
	tags = map[string]model.ID{}
	groups = map[string]model.ID{}
	channels = map[string]model.ID{}
	monitors = map[string]model.ID{}

	tagRows, _, err := t.store.ListTags(ctx, nil, catalogueLimit, "")
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("read existing tags: %w", err)
	}
	for _, row := range tagRows {
		tags[foldName(row.Tag.Name)] = row.Tag.ID
	}

	groupRows, _, err := t.store.ListGroups(ctx, nil, catalogueLimit, "")
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("read existing groups: %w", err)
	}
	for _, row := range groupRows {
		groups[foldName(row.Group.Name)] = row.Group.ID
	}

	channelRows, _, err := t.store.ListChannels(ctx, nil, catalogueLimit, store.ChannelFilter{})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("read existing notification channels: %w", err)
	}
	for _, row := range channelRows {
		channels[foldName(row.Channel.Name)] = row.Channel.ID
	}

	names, err := t.store.MonitorNames(ctx)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("read existing monitor names: %w", err)
	}
	for id, name := range names {
		monitors[foldName(name)] = id
	}
	return tags, groups, channels, monitors, nil
}

// catalogueLimit bounds the pre-read. An install with more than this many tags
// has a taxonomy problem rather than an import problem, and the consequence of
// the bound is a name collision handled as a conflict at write time instead of
// being predicted — which is correct either way, just slower.
const catalogueLimit = 5000

// now is the timestamp every row in one import shares.
//
// One timestamp rather than time.Now() per row, because ADR-004's cursor is
// (updated_at, id) and a thousand monitors created across a two-second import
// would page in an order that has nothing to do with anything. Sharing the
// instant makes the id the tiebreak, which is insertion order.
func importInstant() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }
