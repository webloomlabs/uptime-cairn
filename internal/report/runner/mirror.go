package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/s3"
)

// The offsite mirror.
//
// **A durability copy and never a read path** (ADR-008 item 9), and every choice
// in this file is that sentence applied:
//
//   - The upload happens after the local write and after the row, so a failure
//     has somewhere to be recorded and the artifact is already readable when it
//     is attempted.
//   - A failure is recorded against the artifact and does not degrade the run.
//     A report that rendered, filed and delivered has not failed because a bucket
//     was unreachable, and marking it partial would train an operator to ignore
//     the state that also means "the PDF did not render".
//   - Nothing here is ever consulted to find bytes. One read path is the property
//     being protected, and it is protected by there being no function in this
//     file that returns any.
//
// The state left behind is honest about which of three things happened: no
// mirror configured (empty), configured and not yet up ('pending' or 'failed'),
// or up ('uploaded'). The middle one is a queue the operator can see, which is
// the difference between a mirror that has drifted and a mirror that is believed
// in without evidence.

// Mirror uploads artifacts to an S3-compatible bucket.
//
// One field, because the object-key namespace is already folded into the client's
// configuration as its prefix — there is no second place here where a key is
// assembled, which is what keeps the bucket layout and the disk layout identical.
type Mirror struct {
	client *s3.Client
}

// Uploader is the half of Mirror that the runner depends on, declared by the
// consumer. A nil Uploader is the supported configuration for every install that
// has not enabled a mirror, and the call sites check for it rather than
// constructing a mirror that uploads nowhere.
type Uploader interface {
	Upload(ctx context.Context, relPath string, data []byte, contentType string) error
}

// NewMirror builds a mirror from the report_storage settings section.
//
// Returns nil where the mirror is switched off or incompletely configured, which
// is what makes "no mirror" the same code path as "no mirror configured": the
// runner holds nil and skips, rather than holding an object that fails on every
// artifact.
//
// `secret` is the opened plaintext. It is passed in rather than read here
// because opening the envelope needs the keeper, and the keeper belongs to the
// composition root — the same division applySMTP already makes.
func NewMirror(settings model.ReportStorageSettings, secret string) *Mirror {
	if !settings.MirrorEnabled {
		return nil
	}
	cfg := s3.Config{
		Bucket:               settings.Bucket,
		Prefix:               settings.Prefix,
		Region:               settings.Region,
		Endpoint:             settings.Endpoint,
		PathStyle:            settings.PathStyle,
		AccessKeyID:          settings.AccessKeyID,
		SecretAccessKey:      secret,
		ServerSideEncryption: settings.ServerSideEncryption,
	}
	if err := cfg.Validate(); err != nil {
		// An enabled but incomplete mirror produces no client. The settings
		// surface refuses this combination, so reaching it means the row was
		// edited by hand or written by an older build; either way, uploading to
		// a half-configured bucket is not the recovery.
		return nil
	}
	return &Mirror{client: s3.New(cfg, nil)}
}

// Upload puts one artifact in the bucket under the same relative path it has on
// local disk.
//
// **The same path deliberately.** `2026/09/<artifact-id>.pdf` in the bucket and
// on disk means an operator restoring from the mirror copies a tree into place
// rather than reconstructing a naming rule from the database, and it means the
// two can be compared with a listing. It also inherits the property that makes
// the local path safe: it is derived from the artifact id and the format, so a
// report titled `../../etc` has nowhere to go here either.
func (m *Mirror) Upload(ctx context.Context, relPath string, data []byte, contentType string) error {
	if m == nil || m.client == nil {
		return errors.New("no mirror configured")
	}
	return m.client.Put(ctx, relPath, data, contentType)
}

// mirrorArtifact uploads one artifact and records what happened, without letting
// either outcome reach the run's state.
//
// The error returned describes the *recording*, not the upload: an upload that
// fails is a fact successfully written down, and the caller has nothing to do
// about it. That is what keeps a bucket outage from turning every report on the
// instance into a partial.
func (r *Runner) mirrorArtifact(ctx context.Context, to Uploader, id model.ID, relPath, format string, data []byte) {
	if to == nil {
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	err := to.Upload(ctx, relPath, data, mirrorContentType(format))
	if err == nil {
		if recordErr := r.store.RecordArtifactMirror(ctx, id, model.MirrorUploaded, &now, ""); recordErr != nil {
			// The bytes are offsite and the row says otherwise, which the retry
			// pass will correct by uploading them again. An idempotent PUT makes
			// that safe, which is why the correction is a re-upload rather than
			// a reconciliation.
			r.logMirror("record artifact mirror success", id, recordErr)
		}
		return
	}

	// The provider's own message, kept whole: 'NoSuchBucket' and
	// 'SignatureDoesNotMatch' send an operator to two different screens.
	if recordErr := r.store.RecordArtifactMirror(ctx, id, model.MirrorFailed, nil, err.Error()); recordErr != nil {
		r.logMirror("record artifact mirror failure", id, recordErr)
	}
}

func (r *Runner) logMirror(what string, id model.ID, err error) {
	if r.log == nil {
		return
	}
	r.log.Warn(what, "error", err, "artifact_id", id.String())
}

// mirrorContentType is what the object is served as from a bucket console. The
// same table the download path uses, restated rather than shared because the two
// answer different questions — this one has no charset, since an object store
// hands the header back verbatim and a browser downloading a file has no use for
// one.
func mirrorContentType(format string) string {
	switch format {
	case model.FormatPDF:
		return "application/pdf"
	case model.FormatHTML:
		return "text/html"
	case model.FormatCSV:
		return "text/csv"
	case model.FormatJSON:
		return "application/json"
	}
	return "application/octet-stream"
}

// MirrorState is what an artifact row is created with.
//
// 'pending' when a mirror is configured and empty when it is not, and the
// difference is load-bearing: empty means "no offsite copy was ever intended",
// which is not the same fact as "one is intended and has not happened", and only
// the second belongs in a retry queue.
func mirrorInitialState(configured bool) string {
	if configured {
		return model.MirrorPending
	}
	return ""
}

// --- resolving the mirror per run -------------------------------------------

// Opener unseals the mirror's secret access key. The keeper belongs to the
// composition root, so the runner is handed the narrowest thing it needs rather
// than the keeper itself.
type Opener interface {
	Open(orgID, rowID, envelope []byte) ([]byte, error)
}

// MirrorSource resolves the mirror for one run's settings snapshot.
//
// **Per run rather than at start-up**, and that is a correction rather than a
// preference. The runner already reads the settings row on every execution,
// deliberately: an operator who shortens artifact retention at 09:00 should not
// have to restart the instance for the change to reach the next report. Building
// the mirror at start-up would have reintroduced exactly the drift that rule
// exists to close — an operator enables the mirror, the settings surface accepts
// it, and nothing is ever uploaded until somebody happens to restart.
type MirrorSource interface {
	For(settings model.ReportStorageSettings) Uploader
}

// MirrorProvider resolves and caches one.
//
// The cache is not an optimisation for the upload — a report run is measured in
// seconds and an extra struct allocation is nothing against it. It is there so
// that consecutive runs share one http.Client and therefore one connection pool:
// a fresh client per run would open a new TLS connection for every artifact of
// every report, which at fifty reports on the first of the month is fifty
// handshakes against a bucket that would happily have kept one open.
type MirrorProvider struct {
	opener Opener
	orgID  model.ID

	mu  sync.Mutex
	key string
	// resolved distinguishes "cached as absent" from "not cached yet". Without
	// it a credential that will not open is re-opened for every artifact of every
	// report, which turns one key problem into a log entry per file.
	resolved bool
	current  Uploader

	// log carries the one failure this type can have and cannot report through a
	// return value: a sealed credential that will not open. That is a key
	// problem, and an operator has to be told rather than left with a mirror
	// that is silently off.
	log *slog.Logger
}

func NewMirrorProvider(opener Opener, orgID model.ID, log *slog.Logger) *MirrorProvider {
	return &MirrorProvider{opener: opener, orgID: orgID, log: log}
}

// For returns the mirror these settings describe, or nil for an install that has
// not configured one.
func (p *MirrorProvider) For(settings model.ReportStorageSettings) Uploader {
	if p == nil || !settings.MirrorEnabled {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Keyed on everything that changes the client, the sealed envelope included
	// — a rotated credential has to produce a new client rather than a cache hit
	// on the old one.
	key := fingerprint(settings)
	if p.resolved && key == p.key {
		return p.current
	}

	secret, err := p.opener.Open(p.orgID[:], p.orgID[:], settings.SecretAccessKeySealed)
	if err != nil {
		if p.log != nil {
			p.log.Error("open report storage credential; no offsite copies will be made", "error", err)
		}
		// Cached as absent, so a key problem is logged once rather than once per
		// artifact of every report until somebody fixes it.
		p.key, p.current, p.resolved = key, nil, true
		return nil
	}

	mirror := NewMirror(settings, string(secret))
	if mirror == nil {
		if p.log != nil {
			p.log.Warn("the artifact mirror is enabled but incompletely configured; no offsite copies will be made")
		}
		p.key, p.current, p.resolved = key, nil, true
		return nil
	}
	p.key, p.current, p.resolved = key, mirror, true
	return mirror
}

// fingerprint is every field that would produce a different client.
//
// The sealed envelope rather than the plaintext: this runs before the envelope is
// opened, and putting a live credential into a cache key would be putting one
// into a string that outlives the request.
func fingerprint(s model.ReportStorageSettings) string {
	return strings.Join([]string{
		s.Bucket, s.Prefix, s.Region, s.Endpoint, s.AccessKeyID, s.ServerSideEncryption,
		strconv.FormatBool(s.PathStyle),
		hex.EncodeToString(sha256Sum(s.SecretAccessKeySealed)),
	}, "\x00")
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
