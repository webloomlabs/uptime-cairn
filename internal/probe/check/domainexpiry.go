package check

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// DomainExpiry implements the domain_expiry monitor type.
//
// Two things make this type unlike every other one. It talks to registries,
// which rate-limit and mean it, so the answer is cached for a day and the check
// runs against the cached expiry date — the heartbeat stays current at the
// monitor's own interval while the registry is asked once. And its answer has no
// second source: RDAP is the standard (RFC 9224 bootstrap, RFC 9083 responses)
// and WHOIS is the fallback for the registries that still have not published an
// RDAP endpoint, whose output is unstructured text that has to be pattern-matched
// per registry. Both are implemented here; neither is guessed at.
type DomainExpiry struct {
	client *http.Client

	// bootstrapURL is a field rather than a constant so a test can point it at
	// a local server. Nothing else changes it.
	bootstrapURL string

	mu        sync.Mutex
	bootstrap map[string]string // TLD -> RDAP base URL
	fetchedAt time.Time
	cache     map[string]domainRecord
}

// domainRecord is what was learned about one domain, and when. Caching the date
// rather than the verdict is deliberate: the days-remaining figure on every
// heartbeat is then computed fresh, so it counts down correctly between
// registry lookups instead of going stale for a day at a time.
type domainRecord struct {
	expiry time.Time
	source string
	err    string

	// registrar is empty where the registry's answer did not name one, which
	// thin WHOIS records regularly do not. Recorded rather than looked up
	// separately: it arrives in the same response as the date, and asking a
	// registry a second question for it would double a lookup this type is
	// rate-limited into doing once a day.
	registrar string

	// notRegistered is the one registry answer that is a verdict rather than a
	// failure to read: the name is not registered at all. It is the exact thing
	// this monitor exists to catch, and reporting a lapsed domain as "cannot
	// read the registration" would let the outage it represents pass silently.
	notRegistered bool

	fetchedAt time.Time
}

// registration is what one registry answer yielded. Both lookup paths return it
// so the caller does not have to know which of the two it asked.
type registration struct {
	expiry    time.Time
	registrar string
}

// errNotRegistered is returned by both lookup paths when the registry answers
// that the domain does not exist.
var errNotRegistered = errors.New("domain is not registered")

const (
	// registryInterval is the floor the spec fixes: no more than one registry
	// lookup a day per domain, whatever the monitor's interval says.
	registryInterval = 24 * time.Hour

	// failureInterval is shorter, because a registry that was unreachable an
	// hour ago may well be reachable now — but not so short that a broken
	// endpoint gets hammered at the monitor's interval.
	failureInterval = time.Hour

	// pruneAfter drops entries nothing has asked about in two days, so deleting
	// a monitor eventually returns its memory.
	pruneAfter = 48 * time.Hour

	bootstrapURL      = "https://data.iana.org/rdap/dns.json"
	bootstrapLifetime = 24 * time.Hour

	defaultDomainThreshold = 30
	whoisPort              = "43"
)

// NewDomainExpiry builds the checker.
func NewDomainExpiry() *DomainExpiry {
	return &DomainExpiry{
		client: &http.Client{Transport: &http.Transport{
			ForceAttemptHTTP2: true,
			Proxy:             http.ProxyFromEnvironment,
		}},
		bootstrapURL: bootstrapURL,
		cache:        make(map[string]domainRecord),
	}
}

// Type implements Checker.
func (d *DomainExpiry) Type() string { return model.TypeDomainExpiry }

// Version implements Checker.
func (d *DomainExpiry) Version() uint32 { return 1 }

// domainExpiryConfig mirrors DomainExpiryConfig in docs/api/openapi.yaml.
type domainExpiryConfig struct {
	Domain                 string `json:"domain"`
	DaysRemainingThreshold *int   `json:"days_remaining_threshold"`
	Source                 string `json:"source"`
}

// Validate implements Checker.
func (d *DomainExpiry) Validate(config []byte) error {
	cfg, err := decodeDomainExpiryConfig(config)
	if err != nil {
		return err
	}
	if err := validateHostname(cfg.Domain); err != nil {
		return fmt.Errorf("domain: %w", err)
	}
	if _, err := tldOf(cfg.Domain); err != nil {
		return err
	}
	if cfg.DaysRemainingThreshold != nil {
		if v := *cfg.DaysRemainingThreshold; v < 0 || v > 3650 {
			return fmt.Errorf("days_remaining_threshold %d is outside 0-3650", v)
		}
	}
	switch cfg.Source {
	case "", "auto", "rdap", "whois":
		return nil
	default:
		return fmt.Errorf("source %q: want auto, rdap, or whois", cfg.Source)
	}
}

// Check implements Checker.
func (d *DomainExpiry) Check(ctx context.Context, config []byte) Observation {
	cfg, err := decodeDomainExpiryConfig(config)
	if err != nil {
		return Observation{Status: model.StatusUnknown, Class: ClassConfig, Message: err.Error()}
	}

	threshold := defaultDomainThreshold
	if cfg.DaysRemainingThreshold != nil {
		threshold = *cfg.DaysRemainingThreshold
	}
	domain := strings.ToLower(strings.TrimSuffix(cfg.Domain, "."))

	record := d.lookup(ctx, domain, cfg.Source)
	if record.notRegistered {
		return Observation{
			Status:  model.StatusDown,
			Class:   ClassAssertion,
			Code:    "not_registered",
			Message: domain + " is not registered (per " + record.source + ")",
		}
	}
	if record.err != "" {
		// A registry we could not read tells us nothing about the registration.
		// Unknown, never down: a rate-limited WHOIS server must not open an
		// incident against a domain that has three years left on it.
		return Observation{
			Status:  model.StatusUnknown,
			Class:   ClassNetwork,
			Message: "cannot read the registration for " + domain + ": " + record.err,
		}
	}

	days := int(math.Floor(time.Until(record.expiry).Hours() / 24))
	obs := Observation{
		Status: model.StatusUp,
		Code:   strconv.Itoa(days),
		// Reported on every check, not only on the ones that hit the registry.
		// The date is what was observed and it has not changed since; the
		// control plane decides what is worth writing, and giving it the fact
		// only once a day would leave it deciding from nothing in between.
		Domain: &Domain{
			Domain:                 domain,
			ExpiresAt:              record.expiry.UTC(),
			Registrar:              record.registrar,
			Source:                 strings.ToLower(record.source),
			DaysRemainingThreshold: &threshold,
		},
	}
	expiresOn := record.expiry.UTC().Format(time.RFC3339)

	switch {
	case days < 0:
		obs.Status = model.StatusDown
		obs.Class = ClassAssertion
		obs.Message = fmt.Sprintf("%s expired %s ago, on %s (per %s)", domain, humaniseDays(-days), expiresOn, record.source)
	case days <= threshold:
		obs.Status = model.StatusDown
		obs.Class = ClassAssertion
		obs.Message = fmt.Sprintf("%s expires in %s, on %s — inside the %d-day threshold (per %s)",
			domain, humaniseDays(days), expiresOn, threshold, record.source)
	default:
		obs.Message = fmt.Sprintf("%s registered until %s, %s away (per %s)", domain, expiresOn, humaniseDays(days), record.source)
	}
	return obs
}

// lookup returns the cached registration, refreshing it at most once a day.
func (d *DomainExpiry) lookup(ctx context.Context, domain, source string) domainRecord {
	now := time.Now()

	d.mu.Lock()
	cached, ok := d.cache[domain]
	d.mu.Unlock()

	if ok {
		age := now.Sub(cached.fetchedAt)
		if (cached.err == "" && age < registryInterval) || (cached.err != "" && age < failureInterval) {
			return cached
		}
	}

	record := d.fetch(ctx, domain, source)
	record.fetchedAt = now

	d.mu.Lock()
	d.cache[domain] = record
	for key, entry := range d.cache {
		if now.Sub(entry.fetchedAt) > pruneAfter {
			delete(d.cache, key)
		}
	}
	d.mu.Unlock()

	return record
}

func (d *DomainExpiry) fetch(ctx context.Context, domain, source string) domainRecord {
	var reasons []string

	if source != "whois" {
		found, err := d.viaRDAP(ctx, domain)
		switch {
		case err == nil:
			return domainRecord{expiry: found.expiry, registrar: found.registrar, source: "RDAP"}
		case errors.Is(err, errNotRegistered):
			return domainRecord{notRegistered: true, source: "RDAP"}
		}
		reasons = append(reasons, "RDAP: "+err.Error())
		if source == "rdap" {
			return domainRecord{err: strings.Join(reasons, "; ")}
		}
	}

	found, err := viaWHOIS(ctx, domain)
	switch {
	case err == nil:
		return domainRecord{expiry: found.expiry, registrar: found.registrar, source: "WHOIS"}
	case errors.Is(err, errNotRegistered):
		return domainRecord{notRegistered: true, source: "WHOIS"}
	}
	reasons = append(reasons, "WHOIS: "+err.Error())
	return domainRecord{err: strings.Join(reasons, "; ")}
}

// ---------------------------------------------------------------------------
// RDAP
// ---------------------------------------------------------------------------

// viaRDAP resolves the TLD's RDAP service from the IANA bootstrap registry and
// reads the expiration event. Going through the bootstrap file rather than a
// public redirector keeps the lookup on the path RFC 9224 defines and takes a
// third party out of the middle of every check.
func (d *DomainExpiry) viaRDAP(ctx context.Context, domain string) (registration, error) {
	tld, err := tldOf(domain)
	if err != nil {
		return registration{}, err
	}

	services, err := d.rdapServices(ctx)
	if err != nil {
		return registration{}, err
	}
	base, ok := services[tld]
	if !ok {
		return registration{}, fmt.Errorf(".%s publishes no RDAP service", tld)
	}

	endpoint := strings.TrimSuffix(base, "/") + "/domain/" + domain
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return registration{}, err
	}
	req.Header.Set("Accept", "application/rdap+json")
	req.Header.Set("User-Agent", "uptime-cairn")

	resp, err := d.client.Do(req)
	if err != nil {
		return registration{}, errors.New(redact(err))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return registration{}, err
	}
	if resp.StatusCode == http.StatusNotFound {
		// RFC 9083: the registry answered, and the answer is that this name is
		// not in the registry.
		return registration{}, errNotRegistered
	}
	if resp.StatusCode != http.StatusOK {
		return registration{}, fmt.Errorf("%s answered %s", endpoint, resp.Status)
	}

	var payload struct {
		Events []struct {
			Action string `json:"eventAction"`
			Date   string `json:"eventDate"`
		} `json:"events"`
		Entities []rdapEntity `json:"entities"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return registration{}, fmt.Errorf("decoding response: %w", err)
	}
	for _, event := range payload.Events {
		if event.Action != "expiration" {
			continue
		}
		when, err := parseRegistryDate(event.Date)
		if err != nil {
			return registration{}, fmt.Errorf("expiration event date %q: %w", event.Date, err)
		}
		return registration{expiry: when, registrar: rdapRegistrar(payload.Entities)}, nil
	}
	return registration{}, errors.New("response carries no expiration event")
}

// rdapEntity is the sliver of RFC 9083's entity object this needs: who they are
// and what they are called. Everything else in there — handles, addresses,
// status, nested entities — belongs to a WHOIS-replacement client, which this
// is not.
type rdapEntity struct {
	Roles []string `json:"roles"`

	// VCardArray is ["vcard", [[name, params, type, value], ...]] — a JSON
	// encoding of jCard (RFC 7095) whose entries are heterogeneous arrays, so it
	// is decoded loosely and read defensively rather than modelled.
	VCardArray []json.RawMessage `json:"vcardArray"`
}

// rdapRegistrar returns the display name of the entity holding the registrar
// role, or empty when the response names none.
func rdapRegistrar(entities []rdapEntity) string {
	for _, entity := range entities {
		registrar := false
		for _, role := range entity.Roles {
			if strings.EqualFold(role, "registrar") {
				registrar = true
				break
			}
		}
		if !registrar || len(entity.VCardArray) < 2 {
			continue
		}
		if name := vcardValue(entity.VCardArray[1], "fn"); name != "" {
			return name
		}
	}
	return ""
}

// vcardValue reads one property out of a jCard property array. The shape is
// [[name, params, type, value], ...] and every element is a different type,
// which is why this walks `any` instead of unmarshalling into a struct.
func vcardValue(properties json.RawMessage, want string) string {
	var entries [][]any
	if err := json.Unmarshal(properties, &entries); err != nil {
		return ""
	}
	for _, entry := range entries {
		if len(entry) < 4 {
			continue
		}
		name, ok := entry[0].(string)
		if !ok || !strings.EqualFold(name, want) {
			continue
		}
		if value, ok := entry[3].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// rdapServices returns the bootstrap table, fetched at most once a day.
func (d *DomainExpiry) rdapServices(ctx context.Context) (map[string]string, error) {
	d.mu.Lock()
	if d.bootstrap != nil && time.Since(d.fetchedAt) < bootstrapLifetime {
		table := d.bootstrap
		d.mu.Unlock()
		return table, nil
	}
	d.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.bootstrapURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "uptime-cairn")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching the IANA bootstrap registry: %s", redact(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("IANA bootstrap registry answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}

	// The bootstrap file is an array of [[tlds...], [urls...]] pairs.
	var payload struct {
		Services [][][]string `json:"services"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decoding the IANA bootstrap registry: %w", err)
	}

	table := make(map[string]string, 1500)
	for _, service := range payload.Services {
		if len(service) < 2 || len(service[1]) == 0 {
			continue
		}
		endpoint := service[1][0]
		for _, url := range service[1] {
			// Prefer HTTPS where a registry lists both.
			if strings.HasPrefix(url, "https://") {
				endpoint = url
				break
			}
		}
		for _, tld := range service[0] {
			table[strings.ToLower(tld)] = endpoint
		}
	}
	if len(table) == 0 {
		return nil, errors.New("IANA bootstrap registry listed no services")
	}

	d.mu.Lock()
	d.bootstrap, d.fetchedAt = table, time.Now()
	d.mu.Unlock()
	return table, nil
}

// ---------------------------------------------------------------------------
// WHOIS
// ---------------------------------------------------------------------------

// viaWHOIS asks IANA which server is authoritative for the TLD, then asks that
// server about the domain. Two hops rather than a hard-coded server list, which
// would be wrong within a year.
func viaWHOIS(ctx context.Context, domain string) (registration, error) {
	tld, err := tldOf(domain)
	if err != nil {
		return registration{}, err
	}

	root, err := whoisQuery(ctx, "whois.iana.org", tld)
	if err != nil {
		return registration{}, err
	}
	server := whoisField(root, "whois")
	if server == "" {
		return registration{}, fmt.Errorf(".%s publishes no WHOIS server", tld)
	}

	body, err := whoisQuery(ctx, server, domain)
	if err != nil {
		return registration{}, err
	}

	// Some registries answer with a thin record naming the registrar's own
	// server, which holds the dates. One referral is followed; chains beyond
	// that are a rabbit hole with no defined end.
	if referral := whoisField(body, "registrar whois server"); referral != "" && referral != server {
		if deep, err := whoisQuery(ctx, referral, domain); err == nil {
			if when, err := whoisExpiry(deep); err == nil {
				// The thin record is the fallback for the name, not for the
				// date: a registry that referred us elsewhere for the dates
				// usually still named the registrar itself.
				registrar := whoisRegistrar(deep)
				if registrar == "" {
					registrar = whoisRegistrar(body)
				}
				return registration{expiry: when, registrar: registrar}, nil
			}
		}
	}

	when, err := whoisExpiry(body)
	if err != nil {
		return registration{}, err
	}
	return registration{expiry: when, registrar: whoisRegistrar(body)}, nil
}

func whoisQuery(ctx context.Context, server, query string) (string, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(server, whoisPort))
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write([]byte(query + "\r\n")); err != nil {
		return "", err
	}
	body, err := io.ReadAll(io.LimitReader(conn, maxBody))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// whoisExpiryKeys are the field names registries actually use, lower-cased. The
// list is long because WHOIS has no schema — every entry here is a registry that
// spells it differently, and a missing entry is a monitor that reports unknown
// forever.
var whoisExpiryKeys = []string{
	"registry expiry date",
	"registrar registration expiration date",
	"expiration date",
	"expiration time",
	"expiry date",
	"expires",
	"expire date",
	"expires on",
	"paid-till",
	"renewal date",
	"validity",
}

// whoisNoMatch are the phrases registries use for "this name is not
// registered". They are matched only against a response that carried no expiry
// field at all, so a domain whose registrant name happens to contain one of
// these strings is not mistaken for a lapsed registration.
var whoisNoMatch = []string{
	"no match for",
	"not found",
	"no entries found",
	"no data found",
	"no object found",
	"domain not found",
	"status: free",
	"status: available",
}

// whoisRegistrarKeys are the field names registries use for the sponsoring
// registrar, lower-cased and in preference order. Shorter than the expiry list
// because a missing registrar costs a blank field rather than a monitor that
// never works, so this stops at what the large registries emit.
var whoisRegistrarKeys = []string{
	"registrar",
	"sponsoring registrar",
	"registrar name",
	"registrar organization",
}

func whoisRegistrar(body string) string {
	for _, key := range whoisRegistrarKeys {
		if value := whoisField(body, key); value != "" {
			return value
		}
	}
	return ""
}

func whoisExpiry(body string) (time.Time, error) {
	for _, key := range whoisExpiryKeys {
		value := whoisField(body, key)
		if value == "" {
			continue
		}
		when, err := parseRegistryDate(value)
		if err != nil {
			continue
		}
		return when, nil
	}
	lower := strings.ToLower(body)
	for _, phrase := range whoisNoMatch {
		if strings.Contains(lower, phrase) {
			return time.Time{}, errNotRegistered
		}
	}
	return time.Time{}, errors.New("no expiry date field in the WHOIS response")
}

// whoisField pulls "key: value" out of a WHOIS body, matching the key
// case-insensitively and ignoring the surrounding legal boilerplate.
func whoisField(body, key string) string {
	for _, line := range strings.Split(body, "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(name), key) {
			continue
		}
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// registryDateLayouts covers what RDAP and the WHOIS servers actually emit.
// RDAP is RFC 3339 and would need only the first; the rest are WHOIS.
var registryDateLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05Z0700",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04:05 MST",
	"2006-01-02",
	"02-Jan-2006 15:04:05 MST",
	"02-Jan-2006",
	"02.01.2006 15:04:05",
	"02.01.2006",
	"2006.01.02",
	"January 2 2006",
}

func parseRegistryDate(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	// Several registries append a parenthetical or a timezone word after the
	// date; nothing after the first run of spaces past the time is needed.
	value = strings.TrimSuffix(value, ".")

	for _, layout := range registryDateLayouts {
		if when, err := time.Parse(layout, value); err == nil {
			return when, nil
		}
	}
	return time.Time{}, errors.New("unrecognised date format")
}

// tldOf returns the last label, which is what both the RDAP bootstrap registry
// and IANA's WHOIS are keyed on — including for names under a multi-label
// suffix like co.uk, where the registry is still .uk.
func tldOf(domain string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	idx := strings.LastIndex(trimmed, ".")
	if idx < 0 || idx == len(trimmed)-1 {
		return "", fmt.Errorf("domain %q has no top-level domain", domain)
	}
	if net.ParseIP(trimmed) != nil {
		return "", fmt.Errorf("domain %q is an IP address; domain expiry needs a registered name", domain)
	}
	return trimmed[idx+1:], nil
}

func decodeDomainExpiryConfig(config []byte) (domainExpiryConfig, error) {
	var cfg domainExpiryConfig
	dec := json.NewDecoder(strings.NewReader(string(config)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}
