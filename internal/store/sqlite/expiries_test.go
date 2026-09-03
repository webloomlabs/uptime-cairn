package sqlite

import (
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

var calendarNow = time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

func seedCertificate(t *testing.T, s *Store, id model.ID, subject, issuer string, expires time.Time) {
	t.Helper()
	_, err := s.db.ExecContext(t.Context(), `
		INSERT INTO monitor_certificates (monitor_id, org_id, subject, issuer,
		    valid_from, valid_to, chain_valid, observed_at)
		VALUES (?,?,?,?,?,?,1,?)`,
		id[:], model.SentinelOrgID[:], subject, issuer,
		millis(expires.AddDate(0, -3, 0)), millis(expires), millis(calendarNow))
	if err != nil {
		t.Fatalf("seed certificate: %v", err)
	}
}

func seedDomain(t *testing.T, s *Store, id model.ID, domain, registrar string, expires time.Time) {
	t.Helper()
	_, err := s.db.ExecContext(t.Context(), `
		INSERT INTO monitor_domain_expiry (monitor_id, org_id, domain, expires_at,
		    registrar, source, observed_at)
		VALUES (?,?,?,?,?,'rdap',?)`,
		id[:], model.SentinelOrgID[:], domain, millis(expires), registrar, millis(calendarNow))
	if err != nil {
		t.Fatalf("seed domain: %v", err)
	}
}

// The calendar is one ordered list over two differently shaped tables.
//
// Certificates and registrations are stored apart on purpose — a registration has
// a registrar and a source, and no subject, chain or serial — and the only reader
// that wants them together is this one. Ordering across the union is the thing
// worth testing, because it is the part a per-table query would get right and a
// naive concatenation would get wrong.
func TestTheCalendarOrdersBothKindsTogether(t *testing.T) {
	t.Parallel()

	s := open(t)
	cert := mustCreate(t, s, testMonitor("api.example.com"))
	domain := mustCreate(t, s, testMonitor("example.com"))
	far := mustCreate(t, s, testMonitor("cdn.example.com"))

	seedCertificate(t, s, cert.ID, "api.example.com", "Let's Encrypt", calendarNow.AddDate(0, 0, 20))
	seedDomain(t, s, domain.ID, "example.com", "Example Registrar", calendarNow.AddDate(0, 0, 5))
	seedCertificate(t, s, far.ID, "cdn.example.com", "Let's Encrypt", calendarNow.AddDate(0, 0, 300))

	entries, hasMore, err := s.ListUpcomingExpiries(t.Context(), nil, 25, store.ExpiryFilter{}, calendarNow)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if hasMore {
		t.Error("has_more on a page of three")
	}
	if len(entries) != 3 {
		t.Fatalf("%d entries, want 3", len(entries))
	}

	// Soonest first, across kinds. The domain expires before either certificate.
	if entries[0].Kind != model.ExpiryDomain || entries[0].Subject != "example.com" {
		t.Errorf("first entry = %+v, want the domain that expires in five days", entries[0])
	}
	if entries[0].Issuer != "Example Registrar" {
		t.Errorf("issuer = %q — a registration's registrar shares the column a "+
			"certificate's issuer uses, because a calendar row reads the same way "+
			"either way", entries[0].Issuer)
	}
	if entries[1].Kind != model.ExpiryCertificate || entries[1].DaysRemaining != 20 {
		t.Errorf("second entry = %+v, want the 20-day certificate", entries[1])
	}
	if entries[2].DaysRemaining != 300 {
		t.Errorf("third entry days_remaining = %d, want 300", entries[2].DaysRemaining)
	}
	// The observation date travels, because a calendar built on a stale probe
	// result can be confidently wrong and the reader has to be able to see that.
	if entries[0].ObservedAt.IsZero() {
		t.Error("no observed_at, so a stale row is indistinguishable from a fresh one")
	}
}

// **Something already expired is on the calendar, with a negative count.**
//
// This is the row somebody opened the page to find. A `within_days` filter bounds
// how far ahead to look and says nothing about the past, and flooring the count
// at zero would file "expired eleven days ago" beside "expires today" — which is
// the one distinction that changes what anybody does next.
func TestAnExpiredEntryIsOnTheCalendarWithANegativeCount(t *testing.T) {
	t.Parallel()

	s := open(t)
	lapsed := mustCreate(t, s, testMonitor("old.example.com"))
	seedCertificate(t, s, lapsed.ID, "old.example.com", "Let's Encrypt", calendarNow.AddDate(0, 0, -11))

	within := 30
	entries, _, err := s.ListUpcomingExpiries(t.Context(), nil, 25,
		store.ExpiryFilter{WithinDays: &within}, calendarNow)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries, want 1 — within_days bounds the future, not the past, "+
			"and dropping an expired certificate would leave the screen looking calm "+
			"on the worst possible day", len(entries))
	}
	if entries[0].DaysRemaining != -11 {
		t.Errorf("days_remaining = %d, want -11", entries[0].DaysRemaining)
	}
}

// A certificate with twenty-three hours left reports zero days, not one.
//
// Rounded towards the expiry, which is the pessimistic direction — and the right
// one for a number somebody schedules work against. "One day left" on something
// that dies before tomorrow's stand-up is how a renewal gets missed.
func TestDaysRemainingRoundsTowardsTheExpiry(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := mustCreate(t, s, testMonitor("tomorrow.example.com"))
	seedCertificate(t, s, m.ID, "tomorrow.example.com", "CA", calendarNow.Add(23*time.Hour))

	entries, _, err := s.ListUpcomingExpiries(t.Context(), nil, 25, store.ExpiryFilter{}, calendarNow)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if entries[0].DaysRemaining != 0 {
		t.Errorf("days_remaining = %d for twenty-three hours, want 0", entries[0].DaysRemaining)
	}
}

// within_days bounds the horizon, and the kind filter drops a whole branch.
func TestTheCalendarFiltersByHorizonAndKind(t *testing.T) {
	t.Parallel()

	s := open(t)
	cert := mustCreate(t, s, testMonitor("api"))
	domain := mustCreate(t, s, testMonitor("root"))
	seedCertificate(t, s, cert.ID, "api.example.com", "CA", calendarNow.AddDate(0, 0, 10))
	seedDomain(t, s, domain.ID, "example.com", "Registrar", calendarNow.AddDate(0, 0, 200))

	within := 30
	entries, _, err := s.ListUpcomingExpiries(t.Context(), nil, 25,
		store.ExpiryFilter{WithinDays: &within}, calendarNow)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != model.ExpiryCertificate {
		t.Errorf("within 30 days returned %d entries, want only the certificate", len(entries))
	}

	entries, _, err = s.ListUpcomingExpiries(t.Context(), nil, 25,
		store.ExpiryFilter{Kinds: []string{model.ExpiryDomain}}, calendarNow)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != model.ExpiryDomain {
		t.Errorf("kind=domain returned %d entries, want only the registration", len(entries))
	}
}

// A monitor carrying two of the requested tags appears once.
//
// The same rule MonitorsInScope follows, and for the same reason: an agency
// filtering by two of a client's tags wants that client's certificates, not each
// of them twice.
func TestTagFilteringDoesNotDuplicateAMonitor(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := mustCreate(t, s, testMonitor("api"))
	seedCertificate(t, s, m.ID, "api.example.com", "CA", calendarNow.AddDate(0, 0, 10))

	first := tagNamed(t, s, "acme")
	second := tagNamed(t, s, "production")
	if err := s.SetMonitorTags(t.Context(), m.ID, model.SentinelOrgID, []model.ID{first, second}); err != nil {
		t.Fatalf("tag monitor: %v", err)
	}

	entries, _, err := s.ListUpcomingExpiries(t.Context(), nil, 25,
		store.ExpiryFilter{TagIDs: []model.ID{first, second}}, calendarNow)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("%d entries for one monitor with two matching tags, want 1", len(entries))
	}
}

// The cursor walks the whole calendar exactly once.
//
// Keyset pagination on (expires_at, monitor_id): the property that matters is
// that a page boundary neither repeats nor drops a row, which an offset would get
// wrong the moment a certificate was renewed mid-walk.
func TestThePagedCalendarVisitsEveryEntryOnce(t *testing.T) {
	t.Parallel()

	s := open(t)
	for i := range 7 {
		m := mustCreate(t, s, testMonitor("host"))
		seedCertificate(t, s, m.ID, "host", "CA", calendarNow.AddDate(0, 0, i+1))
	}

	seen := map[model.ID]int{}
	var after *Cursor
	for range 10 {
		entries, hasMore, err := s.ListUpcomingExpiries(t.Context(), after, 2, store.ExpiryFilter{}, calendarNow)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, e := range entries {
			seen[e.MonitorID]++
		}
		if !hasMore {
			break
		}
		last := entries[len(entries)-1]
		after = &Cursor{UpdatedAt: last.ExpiresAt, ID: last.MonitorID}
	}

	if len(seen) != 7 {
		t.Errorf("saw %d distinct entries over the walk, want 7", len(seen))
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("entry %s appeared %d times", id, count)
		}
	}
}

func tagNamed(t *testing.T, s *Store, name string) model.ID {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	tag := model.Tag{
		ID: model.NewID(), OrgID: model.SentinelOrgID,
		Name: name, Slug: name, Color: "#334155",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateTag(t.Context(), tag); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	return tag.ID
}
