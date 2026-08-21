package kuma

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/auth"
	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// tagRef identifies one imported tag by the Kuma tag it came from and the
// per-monitor value it was attached with.
//
// The value is part of the key because data model §10 gap 2 is resolved by
// splitting a valued tag into one tag per value: `env` attached as `production`
// on one monitor and `staging` on another becomes two tags, and a map keyed on
// the Kuma tag id alone would attach whichever one it stored last to every
// monitor that carried either.
type tagRef struct {
	tag   int64
	value string
}

// tagUsedBare reports whether a tag was ever attached with no value.
//
// Without it a tag used with values on some monitors and bare on others loses
// the bare attachments entirely: they would resolve to a variant that does not
// exist, and the monitors carrying it would come across untagged.
func (p *pass) tagUsedBare(ctx context.Context, src *source, tagID int64) bool {
	rows, err := src.query(ctx, "monitor_tag", []string{"tag_id", "value"}, "WHERE tag_id = ?", tagID)
	if err != nil {
		return true
	}
	for _, row := range rows {
		if strings.TrimSpace(text(row["value"])) == "" {
			return true
		}
	}
	return false
}

// The stages, in dependency order: tags, channels, groups, monitors, status
// pages, history. Each references only what earlier stages created, so a
// failure part-way leaves a consistent install rather than monitors pointing at
// channels nobody wrote.

func (p *pass) importTags(ctx context.Context, src *source, ids map[tagRef]model.ID) error {
	rows, err := src.query(ctx, "tag", []string{"id", "name", "color"}, "ORDER BY id")
	if err != nil {
		return err
	}

	// monitor_tag.value is data model §10 gap 2. Kuma lets a tag carry a
	// per-monitor value — `env` attached to one monitor as `production` and to
	// another as `staging` — and our monitor_tags is a bare join.
	//
	// Resolved by synthesising a distinct tag per value rather than by adding a
	// nullable column. Adding the column is cheap in the schema and expensive
	// everywhere else: it appears in the tag filter, in the bulk tag operation,
	// in the status page's tag display, and in the API's tag resource, and every
	// one of those has to decide what a value means. Synthesising "env:
	// production" as its own tag produces something the existing filter, the
	// existing bulk operation, and the existing UI all already handle, and the
	// user can see what happened from the name. The report says so per monitor.
	values := map[int64]map[string]bool{}
	if src.has("monitor_tag", "tag_id", "value") {
		valueRows, err := src.query(ctx, "monitor_tag", []string{"tag_id", "value"}, "")
		if err != nil {
			return err
		}
		for _, row := range valueRows {
			value := strings.TrimSpace(text(row["value"]))
			if value == "" {
				continue
			}
			tagID := number(row["tag_id"])
			if values[tagID] == nil {
				values[tagID] = map[string]bool{}
			}
			values[tagID][value] = true
		}
	}

	for _, row := range rows {
		kumaID := number(row["id"])
		base := strings.TrimSpace(text(row["name"]))
		if base == "" {
			p.record(src.filename, model.ImportEntityTag, strconv.FormatInt(kumaID, 10), "",
				model.ImportResultFailed, "the tag has no name", nil)
			continue
		}

		// One variant per distinct value, plus the bare tag when it was ever
		// attached without one. Each variant is remembered against the value it
		// came from, so a monitor tagged env=staging gets "env: staging" — the
		// bug the obvious one-id-per-Kuma-tag map produces is every monitor
		// receiving whichever variant was stored last, which looks right in the
		// tag list and is wrong on every row.
		variants := map[string]string{"": base}
		var detail string
		if set := values[kumaID]; len(set) > 0 {
			if !p.tagUsedBare(ctx, src, kumaID) {
				delete(variants, "")
			}
			names := make([]string, 0, len(set))
			for value := range set {
				variants[value] = base + ": " + value
				names = append(names, base+": "+value)
			}
			sort.Strings(names)
			detail = fmt.Sprintf(
				"Uptime Kuma attached this tag with a per-monitor value, which this build's tags do not carry; "+
					"it became %d separate tags (%s) so the existing filters and bulk operations work on it unchanged",
				len(names), strings.Join(names, ", "))
		}

		colour := normaliseColour(text(row["color"]))
		var created *model.ID
		result := model.ImportResultImported

		for _, value := range sortedKeys(variants) {
			name := variants[value]
			final, existing, action := p.resolveName(p.tags, name)
			if action == model.ImportResultSkipped {
				ids[tagRef{tag: kumaID, value: value}] = existing
				if created == nil {
					id := existing
					created = &id
				}
				if len(variants) == 1 {
					result = model.ImportResultSkipped
					detail = skipDetail("tag", final)
				}
				continue
			}
			if action == model.ImportResultRenamed {
				result = model.ImportResultRenamed
				if detail == "" {
					detail = fmt.Sprintf("a tag named %q was already here, so this one was imported as %q", name, final)
				}
			}

			tag := model.Tag{
				ID: model.NewID(), OrgID: p.importer.orgID, Name: final,
				Slug: model.Slugify(final), Color: colour,
				CreatedAt: p.at, UpdatedAt: p.at,
			}
			if !p.opts.DryRun {
				if err := p.importer.target.store.CreateTag(ctx, tag); err != nil {
					p.record(src.filename, model.ImportEntityTag, strconv.FormatInt(kumaID, 10), final,
						model.ImportResultFailed, err.Error(), nil)
					continue
				}
			}
			p.tags[foldName(final)] = tag.ID
			ids[tagRef{tag: kumaID, value: value}] = tag.ID
			if created == nil {
				id := tag.ID
				created = &id
			}
		}

		p.record(src.filename, model.ImportEntityTag, strconv.FormatInt(kumaID, 10), base, result, detail, created)
	}
	return nil
}

func (p *pass) importChannels(ctx context.Context, src *source, ids map[int64]model.ID) error {
	rows, err := src.query(ctx, "notification",
		[]string{"id", "name", "active", "is_default", "config"}, "ORDER BY id")
	if err != nil {
		return err
	}

	for _, row := range rows {
		kumaID := number(row["id"])
		sourceID := strconv.FormatInt(kumaID, 10)
		name := strings.TrimSpace(text(row["name"]))
		if name == "" {
			name = "Imported notification " + sourceID
		}

		mapped, err := mapNotification(text(row["config"]))
		if err != nil {
			result := model.ImportResultFailed
			if _, ok := err.(*unsupportedProvider); ok {
				result = model.ImportResultUnsupported
			}
			p.record(src.filename, model.ImportEntityNotification, sourceID, name, result, err.Error(), nil)
			continue
		}

		final, existing, action := p.resolveName(p.channels, name)
		if action == model.ImportResultSkipped {
			ids[kumaID] = existing
			p.record(src.filename, model.ImportEntityNotification, sourceID, name,
				model.ImportResultSkipped, skipDetail("notification channel", final), &existing)
			continue
		}

		detail := strings.Join(mapped.Notes, "; ")
		if action == model.ImportResultRenamed {
			renamed := fmt.Sprintf("a notification channel named %q was already here, so this one was imported as %q", name, final)
			detail = joinDetail(renamed, detail)
		}

		channel := model.NotificationChannel{
			ID: model.NewID(), OrgID: p.importer.orgID, Name: final, Type: mapped.Type,
			Enabled: truthy(row["active"]), IsDefault: truthy(row["is_default"]),
			CreatedAt: p.at, UpdatedAt: p.at,
		}
		if !p.opts.DryRun {
			if err := p.importer.target.createChannel(ctx, channel, mapped.Config); err != nil {
				p.record(src.filename, model.ImportEntityNotification, sourceID, name,
					model.ImportResultFailed, err.Error(), nil)
				continue
			}
		}
		p.channels[foldName(final)] = channel.ID
		ids[kumaID] = channel.ID
		p.record(src.filename, model.ImportEntityNotification, sourceID, name, action, detail, &channel.ID)
	}
	return nil
}

// importMonitors converts groups first, then the monitors under them.
//
// Kuma models a group as a monitor of type `group` with children pointing at it
// through `parent`; we have a real groups table. So the pass runs twice over the
// same rows: once to create a Group for every `group` monitor, and once for
// everything else, resolving each monitor's parent to the group that was made
// for it.
//
// Nesting deeper than one level is flattened, because that is what this build's
// groups do — a group's parent may be a top-level group and no further. The
// report says which monitors moved up.
func (p *pass) importMonitors(ctx context.Context, src *source, tagIDs map[tagRef]model.ID,
	channelIDs, groupIDs, monitorIDs map[int64]model.ID) error {
	rows, err := src.query(ctx, "monitor", monitorColumns, "ORDER BY id")
	if err != nil {
		return err
	}

	monitors := make([]kumaMonitor, 0, len(rows))
	byKumaID := map[int64]kumaMonitor{}
	for _, row := range rows {
		m := readMonitor(row)
		monitors = append(monitors, m)
		byKumaID[m.ID] = m
	}

	// Pass one: groups.
	for _, m := range monitors {
		if m.Type != "group" {
			continue
		}
		sourceID := strconv.FormatInt(m.ID, 10)
		name := m.Name
		if name == "" {
			name = "Imported group " + sourceID
		}

		final, existing, action := p.resolveName(p.groups, name)
		if action == model.ImportResultSkipped {
			groupIDs[m.ID] = existing
			p.record(src.filename, model.ImportEntityMonitor, sourceID, name,
				model.ImportResultSkipped, skipDetail("group", final), &existing)
			continue
		}

		group := model.Group{
			ID: model.NewID(), OrgID: p.importer.orgID, Name: final, Description: m.Desc,
			CreatedAt: p.at, UpdatedAt: p.at,
		}
		var detail string
		if action == model.ImportResultRenamed {
			detail = fmt.Sprintf("a group named %q was already here, so this one was imported as %q", name, final)
		}
		if !p.opts.DryRun {
			if err := p.importer.target.store.CreateGroup(ctx, group); err != nil {
				p.record(src.filename, model.ImportEntityMonitor, sourceID, name,
					model.ImportResultFailed, err.Error(), nil)
				continue
			}
		}
		p.groups[foldName(final)] = group.ID
		groupIDs[m.ID] = group.ID
		p.record(src.filename, model.ImportEntityMonitor, sourceID, name, action,
			joinDetail(detail, "imported as a group: Uptime Kuma models a group as a monitor, and this build has a real group"), &group.ID)
	}

	// The tag and channel attachments, read once for the whole file rather than
	// per monitor. A thousand monitors would otherwise be two thousand queries
	// against somebody's laptop.
	monitorTags := p.tagAttachments(ctx, src)
	monitorChannels := p.attachments(ctx, src, "monitor_notification", "notification_id")

	// Pass two: everything else.
	for _, m := range monitors {
		if m.Type == "group" {
			continue
		}
		sourceID := strconv.FormatInt(m.ID, 10)
		name := m.Name
		if name == "" {
			name = "Imported monitor " + sourceID
		}

		converted, err := mapMonitor(m)
		if err != nil {
			result := model.ImportResultFailed
			if _, ok := err.(*unsupportedType); ok {
				result = model.ImportResultUnsupported
			}
			p.record(src.filename, model.ImportEntityMonitor, sourceID, name, result, err.Error(), nil)
			continue
		}

		// Monitor names collide constantly across merged instances — every Kuma
		// install has a "Checkout". The same conflict strategy applies here as
		// everywhere else, and `rename` is the default because it is the only
		// one of the three that cannot lose one of them.
		final, existing, action := p.resolveName(p.monitors, name)
		var notes []string
		notes = append(notes, converted.Notes...)

		if action == model.ImportResultSkipped {
			// Mapped anyway, so that history and status-page membership attach
			// to the monitor that is already here rather than being dropped.
			// Re-importing history onto it is idempotent: WriteBatch dedupes on
			// (org, monitor, time, probe).
			monitorIDs[m.ID] = existing
			p.record(src.filename, model.ImportEntityMonitor, sourceID, name,
				model.ImportResultSkipped, skipDetail("monitor", final), &existing)
			continue
		}
		if action == model.ImportResultRenamed {
			notes = append(notes, fmt.Sprintf(
				"a monitor named %q was already here, so this one was imported as %q", name, final))
		}

		monitor := model.Monitor{
			ID: model.NewID(), OrgID: p.importer.orgID,
			Name: final, Description: m.Desc, Type: converted.Type,
			// Paused unless the caller asked otherwise, which is the default the
			// spec publishes: a migrating user reviews five thousand imported
			// monitors before five thousand checks start firing at once.
			Enabled:          p.opts.EnableAfterImport && m.Active,
			Interval:         m.Interval,
			Timeout:          m.Timeout,
			Retries:          m.Retries,
			RetryInterval:    m.Retry,
			ResendAfter:      m.Resend,
			UpsideDown:       m.Upside,
			NotifyOnRecovery: true,
			CreatedAt:        p.at,
			UpdatedAt:        p.at,
		}
		monitor.Target = targetOf(converted)

		// The push token is hashed here rather than carried in the config, on
		// the same terms as every other credential: push ingest is
		// unauthenticated and hot, so the hash is what gets looked up through a
		// unique index, and a stolen database must yield no working tokens
		// (data model §12.5).
		if converted.push != nil {
			token := converted.push.token
			if token == "" {
				fresh, err := auth.NewToken()
				if err != nil {
					p.record(src.filename, model.ImportEntityMonitor, sourceID, name,
						model.ImportResultFailed, "could not issue a push token: "+err.Error(), nil)
					continue
				}
				token = fresh
			}
			monitor.PushTokenHash = auth.HashToken(token)
		}

		if m.Parent != nil {
			if groupID, ok := groupIDs[*m.Parent]; ok {
				id := groupID
				monitor.GroupID = &id
			} else if parent, ok := byKumaID[*m.Parent]; ok && parent.Type != "group" {
				notes = append(notes, fmt.Sprintf(
					"in Uptime Kuma this monitor was nested under %q, which is not a group here; "+
						"it was imported at the top level", parent.Name))
			}
		}

		if !p.opts.DryRun {
			if err := p.importer.target.createMonitor(ctx, monitor, converted.Config); err != nil {
				p.record(src.filename, model.ImportEntityMonitor, sourceID, name,
					model.ImportResultFailed, err.Error(), nil)
				continue
			}

			if tags := resolveTags(monitorTags[m.ID], tagIDs); len(tags) > 0 {
				if err := p.importer.target.store.SetMonitorTags(ctx, monitor.ID, monitor.OrgID, tags); err != nil {
					notes = append(notes, "the monitor's tags could not be attached: "+err.Error())
				}
			}
			// An empty channel list is written deliberately rather than left
			// absent: absent means "attach the defaults", and a Kuma monitor
			// with no notifications attached is one the user chose to keep
			// silent. Inheriting this install's defaults would page somebody
			// for a monitor that has never paged anybody.
			channels := resolve(monitorChannels[m.ID], channelIDs)
			if err := p.importer.target.store.SetMonitorChannels(ctx, monitor.ID, monitor.OrgID, channels); err != nil {
				notes = append(notes, "the monitor's notification channels could not be attached: "+err.Error())
			}
		}

		p.monitors[foldName(final)] = monitor.ID
		monitorIDs[m.ID] = monitor.ID
		p.record(src.filename, model.ImportEntityMonitor, sourceID, name,
			action, strings.Join(notes, "; "), &monitor.ID)
	}
	return nil
}

// attachments reads a join table into monitor id → source ids.
func (p *pass) attachments(ctx context.Context, src *source, table, column string) map[int64][]int64 {
	out := map[int64][]int64{}
	if !src.has(table, "monitor_id", column) {
		return out
	}
	rows, err := src.query(ctx, table, []string{"monitor_id", column}, "")
	if err != nil {
		return out
	}
	for _, row := range rows {
		monitor := number(row["monitor_id"])
		out[monitor] = append(out[monitor], number(row[column]))
	}
	return out
}

// tagAttachments reads monitor_tag with its values, so a monitor's tags resolve
// to the variant it was actually attached with.
func (p *pass) tagAttachments(ctx context.Context, src *source) map[int64][]tagRef {
	out := map[int64][]tagRef{}
	if !src.has("monitor_tag", "monitor_id", "tag_id") {
		return out
	}
	rows, err := src.query(ctx, "monitor_tag", []string{"monitor_id", "tag_id", "value"}, "")
	if err != nil {
		return out
	}
	for _, row := range rows {
		monitor := number(row["monitor_id"])
		out[monitor] = append(out[monitor], tagRef{
			tag:   number(row["tag_id"]),
			value: strings.TrimSpace(text(row["value"])),
		})
	}
	return out
}

// resolveTags maps a monitor's tag attachments onto what was imported.
func resolveTags(refs []tagRef, ids map[tagRef]model.ID) []model.ID {
	out := make([]model.ID, 0, len(refs))
	seen := map[model.ID]bool{}
	for _, ref := range refs {
		id, ok := ids[ref]
		if !ok {
			// A value not seen during the tag pass — possible when the tag
			// itself failed to import. Falls back to the bare variant rather
			// than dropping the attachment silently.
			id, ok = ids[tagRef{tag: ref.tag}]
		}
		if ok && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// resolve maps a monitor's source-side attachments onto what was imported.
// Anything that did not import is left out rather than nil-padded.
func resolve(sourceIDs []int64, ids map[int64]model.ID) []model.ID {
	out := make([]model.ID, 0, len(sourceIDs))
	seen := map[model.ID]bool{}
	for _, sourceID := range sourceIDs {
		if id, ok := ids[sourceID]; ok && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func (p *pass) importStatusPages(ctx context.Context, src *source, monitorIDs map[int64]model.ID) error {
	rows, err := src.query(ctx, "status_page", []string{
		"id", "slug", "title", "description", "theme", "published", "footer_text",
		"custom_css", "show_powered_by", "google_analytics_tag_id", "show_tags", "password",
	}, "ORDER BY id")
	if err != nil {
		return err
	}

	// Kuma's `group` table is its status-page sections, and `monitor_group` is
	// the membership. Both read once for the file.
	sections := map[int64][]map[string]any{}
	if src.has("group", "id", "status_page_id") {
		groupRows, err := src.query(ctx, "group",
			[]string{"id", "name", "status_page_id", "weight", "public"}, "ORDER BY weight, id")
		if err != nil {
			return err
		}
		for _, row := range groupRows {
			page := number(row["status_page_id"])
			sections[page] = append(sections[page], row)
		}
	}
	members := map[int64][]int64{}
	if src.has("monitor_group", "group_id", "monitor_id") {
		memberRows, err := src.query(ctx, "monitor_group",
			[]string{"group_id", "monitor_id", "weight"}, "ORDER BY weight, id")
		if err != nil {
			return err
		}
		for _, row := range memberRows {
			group := number(row["group_id"])
			members[group] = append(members[group], number(row["monitor_id"]))
		}
	}

	for _, row := range rows {
		kumaID := number(row["id"])
		sourceID := strconv.FormatInt(kumaID, 10)
		title := strings.TrimSpace(text(row["title"]))
		slug := strings.ToLower(strings.TrimSpace(text(row["slug"])))
		if title == "" {
			title = "Imported status page " + sourceID
		}
		if slug == "" {
			slug = model.Slugify(title)
		}

		// A slug is a URL somebody may have bookmarked, so a collision is not
		// resolved by suffixing silently — it is resolved and named, because the
		// old bookmark now points at the wrong page either way and the user has
		// to know which.
		// The slug check runs on a dry run too: the whole point of a dry run is
		// to find out what would happen, and "the slug you have been using for
		// two years is taken" is the finding most worth having before the fact.
		var notes []string
		final := slug
		{
			if _, err := p.importer.target.store.StatusPageBySlug(ctx, slug); err == nil {
				if p.opts.ConflictStrategy == ConflictSkip || p.opts.ConflictStrategy == ConflictReplace {
					p.record(src.filename, model.ImportEntityStatusPage, sourceID, title,
						model.ImportResultSkipped,
						fmt.Sprintf("a status page already answers on /status/%s, and the conflict strategy is to keep it", slug), nil)
					continue
				}
				for n := 2; n < 1000; n++ {
					candidate := slug + "-" + strconv.Itoa(n)
					if _, err := p.importer.target.store.StatusPageBySlug(ctx, candidate); err != nil {
						final = candidate
						break
					}
				}
				notes = append(notes, fmt.Sprintf(
					"a status page already answers on /status/%s, so this one is at /status/%s — "+
						"any bookmark or link pointing at the old path now reaches the other page", slug, final))
			}
		}

		page := model.StatusPage{
			ID: model.NewID(), OrgID: p.importer.orgID,
			Slug: final, Title: p.prefixed(title), Description: text(row["description"]),
			Published:  truthy(row["published"]),
			Visibility: "public",
			Theme:      normaliseTheme(text(row["theme"])),
			FooterText: text(row["footer_text"]),
			CustomCSS:  text(row["custom_css"]),
			// Kuma's column is show_powered_by and means what it says, so this
			// is a copy rather than an inversion — named because the adjacent
			// `published` reads the same way and the pair is easy to mistype.
			ShowPoweredBy:        truthy(row["show_powered_by"]),
			GoogleAnalyticsID:    text(row["google_analytics_tag_id"]),
			ShowUptimePercentage: true,
			UptimeBarDays:        90,
			CreatedAt:            p.at,
			UpdatedAt:            p.at,
		}

		position := 0
		placed := map[model.ID]bool{}
		for _, section := range sections[kumaID] {
			groupID := number(section["id"])
			var ids []model.ID
			for _, sourceMonitor := range members[groupID] {
				id, ok := monitorIDs[sourceMonitor]
				if !ok || placed[id] {
					// A monitor may appear in at most one section per page, which
					// the schema enforces. Kuma allows the same monitor in two
					// groups, so the first placement wins and the rest are
					// dropped rather than failing the write.
					continue
				}
				placed[id] = true
				ids = append(ids, id)
			}
			name := strings.TrimSpace(text(section["name"]))
			if name == "" {
				name = "Services"
			}
			page.Sections = append(page.Sections, model.StatusPageSection{
				ID: model.NewID(), StatusPageID: page.ID, OrgID: p.importer.orgID,
				Name: name, Position: position, MonitorIDs: ids,
			})
			position++
		}

		if !p.opts.DryRun {
			if err := p.importer.target.store.CreateStatusPage(ctx, page); err != nil {
				p.record(src.filename, model.ImportEntityStatusPage, sourceID, title,
					model.ImportResultFailed, err.Error(), nil)
				continue
			}
		}
		result := model.ImportResultImported
		if final != slug {
			result = model.ImportResultRenamed
		}
		if password := text(row["password"]); password != "" {
			notes = append(notes, "the page's password was not imported: Uptime Kuma stores it in a form "+
				"this build cannot verify against, so the page was imported public — set a password on it if it needs one")
		}
		p.record(src.filename, model.ImportEntityStatusPage, sourceID, title, result,
			strings.Join(notes, "; "), &page.ID)
	}
	return nil
}

// importHistory copies raw heartbeats.
//
// Off by default and slower, which is what the spec says. Two things about it
// are worth stating rather than discovering.
//
// The timestamps are Kuma's, which it writes as local time without a zone in
// 1.x. They are read as UTC, so an imported history can be offset by the source
// host's own offset — a real inaccuracy, recorded in the report against the
// range rather than left for somebody to notice as a downtime that happened at
// the wrong hour.
//
// And it writes through WriteBatch, which is idempotent on
// (org, monitor, time, probe). Running the same import twice therefore produces
// one history rather than two, which matters because "it did not look right, I
// ran it again" is what people do.
func (p *pass) importHistory(ctx context.Context, src *source, monitorIDs map[int64]model.ID) error {
	if !src.has("heartbeat", "monitor_id", "status", "time") {
		return nil
	}

	for _, sourceID := range sortedMonitorKeys(monitorIDs) {
		target := monitorIDs[sourceID]
		rows, err := src.query(ctx, "heartbeat",
			[]string{"monitor_id", "status", "msg", "time", "ping", "important"},
			"WHERE monitor_id = ? ORDER BY time", sourceID)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			continue
		}

		beats := make([]model.Heartbeat, 0, len(rows))
		var earliest, latest time.Time
		for _, row := range rows {
			at, ok := timestamp(row["time"])
			if !ok {
				continue
			}
			beat := model.Heartbeat{
				Time: at, MonitorID: target, OrgID: p.importer.orgID,
				ProbeID:   model.EmbeddedProbeID,
				Status:    kumaStatus(number(row["status"])),
				Message:   text(row["msg"]),
				Attempt:   1,
				Important: truthy(row["important"]),
			}
			if ping := decimal(row["ping"]); ping > 0 {
				d := time.Duration(ping * float64(time.Millisecond))
				beat.ResponseTime = &d
			}
			beats = append(beats, beat)
			if earliest.IsZero() || at.Before(earliest) {
				earliest = at
			}
			if at.After(latest) {
				latest = at
			}
		}
		if len(beats) == 0 {
			continue
		}

		detail := fmt.Sprintf(
			"%d checks from %s to %s. Uptime Kuma records these without a time zone, so they are read as UTC "+
				"and may be offset by the source host's own offset",
			len(beats), earliest.Format(time.RFC3339), latest.Format(time.RFC3339))

		if !p.opts.DryRun {
			// Chunked, because a monitor with a year of twenty-second checks has
			// well over a million rows and one statement holding all of them is
			// a memory profile nobody budgeted for.
			for start := 0; start < len(beats); start += historyBatch {
				end := min(start+historyBatch, len(beats))
				if _, err := p.importer.target.store.WriteBatch(ctx, beats[start:end]); err != nil {
					p.record(src.filename, model.ImportEntityHeartbeatRange,
						strconv.FormatInt(sourceID, 10), "", model.ImportResultFailed, err.Error(), &target)
					detail = ""
					break
				}
			}
		}
		if detail != "" {
			id := target
			p.record(src.filename, model.ImportEntityHeartbeatRange, strconv.FormatInt(sourceID, 10), "",
				model.ImportResultImported, detail, &id)
		}
	}
	return nil
}

// historyBatch is how many heartbeats go in one write. Large enough that the
// per-statement overhead disappears, small enough that a year of history for one
// monitor does not have to fit in memory as one slice of parameters.
const historyBatch = 1000

// kumaStatus maps Kuma's heartbeat status onto ours.
//
// Kuma: 0 down, 1 up, 2 pending, 3 maintenance. Ours differs and the numbers
// happen to line up for three of the four — which is exactly the situation
// where a cast looks correct and silently inverts something later, so it is
// spelled out.
func kumaStatus(status int64) model.Status {
	switch status {
	case 1:
		return model.StatusUp
	case 2:
		return model.StatusPending
	case 3:
		return model.StatusMaintenance
	default:
		return model.StatusDown
	}
}

func sortedMonitorKeys(m map[int64]model.ID) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// targetOf promotes the host out of a config, so "what else points at this
// host?" is an indexed query on imported monitors too.
func targetOf(m mapped) string {
	for _, key := range []string{"url", "hostname", "address", "domain", "container"} {
		if value, ok := m.Config[key]; ok {
			if s, ok := value.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// normaliseColour keeps Kuma's tag colours where they are already hex triples
// and falls back to the schema's default where they are not — Kuma also stores
// Tailwind class names in this column.
func normaliseColour(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) == 7 && strings.HasPrefix(raw, "#") {
		return strings.ToLower(raw)
	}
	return "#6b7280"
}

func normaliseTheme(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "dark":
		return "dark"
	case "light":
		return "light"
	default:
		return "auto"
	}
}

// joinDetail concatenates the non-empty parts of a report detail.
func joinDetail(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "; ")
}
