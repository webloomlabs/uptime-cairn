package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/report"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Report schedules: when a template fires and where its output goes.
//
// The one thing this file is careful about is **refusing a schedule that will
// never fire**. `next_run_at` is computed on every write, and a definition with
// no next firing — the 30th of February, a zone that no longer exists — is a 422
// rather than a stored row. A schedule that silently does nothing is discovered
// by its silence, usually by a client asking where the report went.

// ReportScheduleStore is the scheduling half of persistence.
type ReportScheduleStore interface {
	CreateReportSchedule(ctx context.Context, s model.ReportSchedule, targets []model.ReportScheduleDelivery) error
	UpdateReportSchedule(ctx context.Context, s model.ReportSchedule, targets []model.ReportScheduleDelivery) error
	GetReportSchedule(ctx context.Context, id model.ID) (model.ReportSchedule, error)
	ListReportSchedules(ctx context.Context, after *store.Cursor, limit int, templateID *model.ID) ([]model.ReportSchedule, bool, error)
	DeleteReportSchedule(ctx context.Context, id model.ID, at time.Time) error
	DeliveriesForSchedule(ctx context.Context, scheduleID model.ID) ([]model.ReportScheduleDelivery, error)
}

func (s *Server) listReportSchedules(w http.ResponseWriter, r *http.Request) {
	after, ok := s.cursor(w, r)
	if !ok {
		return
	}

	var templateID *model.ID
	if raw := r.URL.Query().Get("report_template_id"); raw != "" {
		id, valid := model.ParseID(raw)
		if !valid {
			writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
				"Validation failed", "The schedule list could not be filtered.",
				ValidationItem{Pointer: "/report_template_id", Code: "invalid", Message: "not a uuid"})
			return
		}
		templateID = &id
	}

	schedules, hasMore, err := s.store.ListReportSchedules(r.Context(), after, s.limit(r), templateID)
	if err != nil {
		s.internal(w, r, "list report schedules", err)
		return
	}

	body := page[reportScheduleJSON]{Data: []reportScheduleJSON{}, Pagination: pagination{HasMore: hasMore}}
	for _, sched := range schedules {
		targets, err := s.store.DeliveriesForSchedule(r.Context(), sched.ID)
		if err != nil {
			s.internal(w, r, "list schedule deliveries", err)
			return
		}
		body.Data = append(body.Data, toReportScheduleJSON(sched, targets))
	}
	if hasMore && len(schedules) > 0 {
		last := schedules[len(schedules)-1]
		next := store.Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}.Encode()
		body.Pagination.NextCursor = &next
	}
	writeJSON(w, s.log, http.StatusOK, body)
}

func (s *Server) createReportSchedule(w http.ResponseWriter, r *http.Request) {
	var body reportScheduleWrite
	if !s.readBody(w, r, maxReportBody, &body) {
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	sched := model.ReportSchedule{
		ID:        model.NewID(),
		OrgID:     s.orgID,
		Enabled:   true,
		Frequency: model.ReportFrequencyMonthly,
		Timezone:  s.reportTimezone().String(),
		SendAt:    "09:00",
		CreatedAt: now,
		UpdatedAt: now,
	}

	targets, problems := s.applySchedule(r.Context(), &sched, body, now)
	if body.ReportTemplateID == nil {
		problems = append(problems, ValidationItem{Pointer: "/report_template_id",
			Code: "required", Message: "report_template_id is required"})
	}
	if body.Deliveries == nil || len(*body.Deliveries) == 0 {
		problems = append(problems, ValidationItem{Pointer: "/deliveries", Code: "required",
			Message: "a schedule needs at least one delivery target"})
	}
	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The report schedule was not created.", problems...)
		return
	}

	if err := s.store.CreateReportSchedule(r.Context(), sched, targets); err != nil {
		s.internal(w, r, "create report schedule", err)
		return
	}
	writeJSON(w, s.log, http.StatusCreated, toReportScheduleJSON(sched, targets))
}

func (s *Server) getReportSchedule(w http.ResponseWriter, r *http.Request) {
	sched, targets, ok := s.loadSchedule(w, r)
	if !ok {
		return
	}
	writeJSON(w, s.log, http.StatusOK, toReportScheduleJSON(sched, targets))
}

func (s *Server) updateReportSchedule(w http.ResponseWriter, r *http.Request) {
	sched, existing, ok := s.loadSchedule(w, r)
	if !ok {
		return
	}
	var body reportScheduleWrite
	if !s.readBody(w, r, maxReportBody, &body) {
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	targets, problems := s.applySchedule(r.Context(), &sched, body, now)
	if body.Deliveries == nil {
		// Absent means "leave them", so the stored targets are rewritten
		// unchanged — the store replaces wholesale and would otherwise clear
		// them.
		targets = existing
	} else if len(targets) == 0 {
		problems = append(problems, ValidationItem{Pointer: "/deliveries", Code: "invalid",
			Message: "a schedule needs at least one delivery target"})
	}
	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The report schedule was not updated.", problems...)
		return
	}

	sched.UpdatedAt = now
	if err := s.store.UpdateReportSchedule(r.Context(), sched, targets); err != nil {
		s.reportStoreError(w, r, "update report schedule", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, toReportScheduleJSON(sched, targets))
}

func (s *Server) deleteReportSchedule(w http.ResponseWriter, r *http.Request) {
	id, ok := s.reportScheduleID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteReportSchedule(r.Context(), id, time.Now().UTC().Truncate(time.Millisecond)); err != nil {
		s.reportStoreError(w, r, "delete report schedule", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) loadSchedule(w http.ResponseWriter, r *http.Request) (model.ReportSchedule, []model.ReportScheduleDelivery, bool) {
	id, ok := s.reportScheduleID(w, r)
	if !ok {
		return model.ReportSchedule{}, nil, false
	}
	sched, err := s.store.GetReportSchedule(r.Context(), id)
	if err != nil {
		s.reportStoreError(w, r, "get report schedule", err)
		return model.ReportSchedule{}, nil, false
	}
	targets, err := s.store.DeliveriesForSchedule(r.Context(), id)
	if err != nil {
		s.internal(w, r, "list schedule deliveries", err)
		return model.ReportSchedule{}, nil, false
	}
	return sched, targets, true
}

func (s *Server) reportScheduleID(w http.ResponseWriter, r *http.Request) (model.ID, bool) {
	id, ok := model.ParseID(r.PathValue("reportScheduleId"))
	if !ok {
		s.reportNotFound(w, r)
		return model.ID{}, false
	}
	return id, true
}

// applySchedule merges a write body onto a schedule and recomputes its firing
// time, returning the delivery targets and whatever it could not accept.
func (s *Server) applySchedule(ctx context.Context, sched *model.ReportSchedule, body reportScheduleWrite, now time.Time) ([]model.ReportScheduleDelivery, []ValidationItem) {
	var problems []ValidationItem

	if body.ReportTemplateID != nil {
		id, valid := model.ParseID(*body.ReportTemplateID)
		if !valid {
			problems = append(problems, ValidationItem{Pointer: "/report_template_id",
				Code: "invalid", Message: "not a uuid"})
		} else if _, err := s.store.GetReportTemplate(ctx, id); err != nil {
			// Checked here rather than left to the foreign key, so the answer
			// names the field instead of arriving as an internal error.
			problems = append(problems, ValidationItem{Pointer: "/report_template_id",
				Code: "not_found", Message: "no report template with that identifier"})
		} else {
			sched.ReportTemplateID = id
		}
	}
	if body.Name != nil {
		sched.Name = *body.Name
	}
	if body.Enabled != nil {
		sched.Enabled = *body.Enabled
	}
	if body.Frequency != nil {
		if !oneOf(*body.Frequency, model.ReportFrequencyDaily, model.ReportFrequencyWeekly,
			model.ReportFrequencyMonthly, model.ReportFrequencyQuarterly, model.ReportFrequencyCron) {
			problems = append(problems, ValidationItem{Pointer: "/frequency", Code: "invalid",
				Message: "frequency must be daily, weekly, monthly, quarterly or cron"})
		} else {
			sched.Frequency = *body.Frequency
		}
	}
	if body.Cron != nil {
		sched.Cron = *body.Cron
	}
	if body.Timezone != nil {
		sched.Timezone = *body.Timezone
	}
	if body.SendAt != nil {
		sched.SendAt = *body.SendAt
	}

	targets, targetProblems := s.parseDeliveryTargets(sched, body, now)
	problems = append(problems, targetProblems...)

	// **The firing time is computed on every write, and a schedule with none is
	// refused.** This is where "the 30th of February" and "a zone that no longer
	// exists" stop being stored rows that quietly never run.
	next, err := report.NextRun(report.ScheduleSpec{
		Frequency: sched.Frequency,
		Cron:      sched.Cron,
		Timezone:  sched.Timezone,
		SendAt:    sched.SendAt,
	}, now)
	if err != nil {
		problems = append(problems, ValidationItem{Pointer: "/frequency", Code: "invalid", Message: err.Error()})
	} else {
		sched.NextRunAt = &next
	}
	return targets, problems
}

// parseDeliveryTargets turns the write body's targets into rows.
//
// A method rather than a free function because an s3 target carries a credential
// and sealing one needs the keeper. That is the only reason, and it is worth
// stating: everything else here is pure validation, and the seal is the single
// line that made it impure.
func (s *Server) parseDeliveryTargets(sched *model.ReportSchedule, body reportScheduleWrite, now time.Time) ([]model.ReportScheduleDelivery, []ValidationItem) {
	if body.Deliveries == nil {
		return nil, nil
	}

	var (
		out      []model.ReportScheduleDelivery
		problems []ValidationItem
	)

	for i, incoming := range *body.Deliveries {
		pointer := fmt.Sprintf("/deliveries/%d", i)
		if !oneOf(incoming.Type, model.ReportDeliveryEmail, model.ReportDeliverySlack,
			model.ReportDeliveryWebhook, model.ReportDeliveryS3) {
			problems = append(problems, ValidationItem{Pointer: pointer + "/type", Code: "invalid",
				Message: "type must be email, slack, webhook or s3"})
			continue
		}
		for _, format := range incoming.Formats {
			if !oneOf(format, model.FormatPDF, model.FormatHTML, model.FormatCSV, model.FormatJSON) {
				problems = append(problems, ValidationItem{Pointer: pointer + "/formats", Code: "invalid",
					Message: "format must be pdf, html, csv or json"})
			}
		}

		target := model.ReportScheduleDelivery{
			ID:               model.NewID(),
			OrgID:            sched.OrgID,
			ReportScheduleID: sched.ID,
			Type:             incoming.Type,
			Formats:          incoming.Formats,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if incoming.NotificationChannelID != nil && *incoming.NotificationChannelID != "" {
			id, valid := model.ParseID(*incoming.NotificationChannelID)
			if !valid {
				problems = append(problems, ValidationItem{Pointer: pointer + "/notification_channel_id",
					Code: "invalid", Message: "not a uuid"})
			} else {
				target.NotificationChannelID = &id
			}
		}

		// Non-secret configuration only. The split is at the storage boundary
		// rather than the API boundary, which is what makes it impossible for a
		// read path serialising Config to leak a credential: the credential is
		// not in Config to leak.
		config := map[string]any{}
		if len(incoming.Recipients) > 0 {
			config["recipients"] = incoming.Recipients
		}
		if incoming.URL != nil && *incoming.URL != "" {
			config["url"] = *incoming.URL
		}
		if incoming.Type == model.ReportDeliveryS3 {
			problems = append(problems, s.applyS3Target(&target, config, incoming.S3, pointer)...)
		}
		encoded, err := json.Marshal(config)
		if err != nil {
			problems = append(problems, ValidationItem{Pointer: pointer, Code: "invalid",
				Message: "delivery configuration could not be encoded"})
			continue
		}
		target.Config = encoded
		out = append(out, target)
	}
	return out, problems
}

// applyS3Target folds a drop's configuration onto the row, sealing its secret.
//
// **The drop, not the mirror.** They share the client in internal/s3 and nothing
// else: the mirror is a durability copy of every artifact, configured once in
// settings, and this drops one schedule's output into a bucket for a recipient.
// The two are kept apart by name here, in the settings section, and in the docs,
// because an operator who configures this and believes they have durability has
// bought exactly nothing against the failure a mirror exists for.
//
// The credential lands in `target.SecretsSealed`, which is a different field from
// `Config` and is the whole of the reason a read path cannot leak it: the write
// path never puts a secret in the map that the read path serialises.
func (s *Server) applyS3Target(
	target *model.ReportScheduleDelivery,
	config map[string]any,
	body *s3TargetWrite,
	pointer string,
) []ValidationItem {
	var problems []ValidationItem
	bad := func(field, code, message string) {
		problems = append(problems, ValidationItem{Pointer: pointer + "/s3/" + field, Code: code, Message: message})
	}

	if body == nil {
		problems = append(problems, ValidationItem{Pointer: pointer + "/s3", Code: "required",
			Message: "an s3 delivery target needs an s3 block"})
		return problems
	}

	set := func(field string, value *string) string {
		if value == nil {
			return ""
		}
		trimmed := strings.TrimSpace(*value)
		if trimmed != "" {
			config[field] = trimmed
		}
		return trimmed
	}

	bucket := set("bucket", body.Bucket)
	set("prefix", body.Prefix)
	region := set("region", body.Region)
	endpoint := set("endpoint", body.Endpoint)
	if body.PathStyle != nil && *body.PathStyle {
		config["path_style"] = true
	}
	accessKey := set("access_key_id", body.AccessKeyID)

	if bucket == "" {
		bad("bucket", "required", "bucket is required")
	}
	if region == "" {
		// Named in these words because an operator on MinIO will reasonably
		// leave it blank, never having needed one — and the failure without it
		// is a 403 that explains nothing.
		bad("region", "required", "region is required for the request signature, even where the provider ignores it")
	}
	if endpoint != "" {
		parsed, err := url.ParseRequestURI(endpoint)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			bad("endpoint", "invalid", "endpoint must be an absolute http or https URL")
		}
	}
	if accessKey == "" {
		bad("access_key_id", "required", "access_key_id is required")
	}

	// The key id goes in Config because it names a principal rather than
	// authenticating one; the secret does not, because it does. Config is what
	// the read path serialises.
	if body.SecretAccessKey == nil || *body.SecretAccessKey == "" {
		bad("secret_access_key", "required", "secret_access_key is required")
		return problems
	}
	if *body.SecretAccessKey == model.Redacted {
		bad("secret_access_key", "redacted",
			"supply the real secret access key: a read of this schedule never returns one, so a redaction marker here was typed")
		return problems
	}

	sealed, err := s.reportDrops.Seal(target.OrgID[:], target.ID[:], []byte(*body.SecretAccessKey))
	if err != nil {
		s.log.Error("seal report delivery secret", "error", err)
		bad("secret_access_key", "unavailable", "the secret access key could not be stored")
		return problems
	}
	target.SecretsSealed = sealed
	return problems
}
