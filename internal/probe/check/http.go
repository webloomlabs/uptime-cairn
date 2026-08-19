package check

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// maxBody bounds what a check will read. A monitor pointed at a multi-gigabyte
// download must not take the process down with it, and no assertion this checker
// supports needs more than the first megabyte.
const maxBody = 1 << 20

// HTTP implements the http monitor type.
//
// Fresh connection per check, per docs/probe/protocol.md §6.1: a pooled
// connection can report up against a socket the far end has already half-closed,
// and it measures none of DNS, TCP, or TLS — which is precisely what the user
// thinks they are buying.
type HTTP struct {
	verifying *http.Transport
	skipping  *http.Transport
}

// NewHTTP builds the checker. Two transports rather than one per check: a
// Transport carries connection state and building one per request would allocate
// far more than the connection it refuses to reuse.
func NewHTTP() *HTTP {
	base := func(verify bool) *http.Transport {
		return &http.Transport{
			DisableKeepAlives: true,
			ForceAttemptHTTP2: true,
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: !verify}, //nolint:gosec // verify_tls=false is an explicit, UI-warned per-monitor choice for private CAs
			Proxy:             http.ProxyFromEnvironment,
		}
	}
	return &HTTP{verifying: base(true), skipping: base(false)}
}

// Type implements Checker.
func (h *HTTP) Type() string { return model.TypeHTTP }

// Version implements Checker. Bump it when this checker starts honouring a
// config field it previously ignored, so a control plane can withhold monitors
// an older probe would silently under-check.
func (h *HTTP) Version() uint32 { return 2 }

// httpConfig mirrors HttpConfig in docs/api/openapi.yaml. Pointers where the
// spec's default is true, so "unset" and "explicitly false" stay distinguishable.
type httpConfig struct {
	URL                 string            `json:"url"`
	Method              string            `json:"method"`
	Headers             map[string]string `json:"headers"`
	Body                *string           `json:"body"`
	BodyEncoding        string            `json:"body_encoding"`
	AcceptedStatusCodes []string          `json:"accepted_status_codes"`
	Keyword             *struct {
		Value         string `json:"value"`
		Mode          string `json:"mode"`
		CaseSensitive bool   `json:"case_sensitive"`
	} `json:"keyword"`
	JSONPath                json.RawMessage `json:"json_path"`
	ResponseTimeThresholdMs *int            `json:"response_time_threshold_ms"`
	FollowRedirects         *bool           `json:"follow_redirects"`
	MaxRedirects            *int            `json:"max_redirects"`
	VerifyTLS               *bool           `json:"verify_tls"`
	Auth                    *struct {
		Type     string `json:"type"`
		Username string `json:"username"`
		Password string `json:"password"`
		Token    string `json:"token"`
	} `json:"auth"`
	IPFamily string `json:"ip_family"`
}

var validMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodHead: true,
	http.MethodOptions: true,
}

// Validate runs at assignment time, not check time. A monitor whose config the
// probe cannot honour is reported once, immediately, as a configuration error
// the user can see — not discovered 250 times a second.
func (h *HTTP) Validate(config []byte) error {
	var cfg httpConfig
	dec := json.NewDecoder(strings.NewReader(string(config)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	if cfg.URL == "" {
		return errors.New("url is required")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return fmt.Errorf("url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url scheme %q: want http or https", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("url has no host")
	}

	if cfg.Method != "" && !validMethods[cfg.Method] {
		return fmt.Errorf("method %q is not one the spec allows", cfg.Method)
	}
	if _, err := parseStatusRanges(cfg.AcceptedStatusCodes); err != nil {
		return err
	}
	if cfg.Keyword != nil {
		if cfg.Keyword.Value == "" {
			return errors.New("keyword assertion requires a non-empty value")
		}
		switch cfg.Keyword.Mode {
		case "contains", "not_contains":
		case "regex", "not_regex":
			if _, err := compileKeyword(cfg.Keyword.Value, cfg.Keyword.CaseSensitive); err != nil {
				return fmt.Errorf("keyword regex: %w", err)
			}
		default:
			return fmt.Errorf("keyword mode %q: want contains, not_contains, regex, or not_regex", cfg.Keyword.Mode)
		}
	}
	if hasJSONPath(cfg) {
		if _, err := parseJSONPathAssertion(cfg.JSONPath); err != nil {
			return err
		}
	}
	if cfg.ResponseTimeThresholdMs != nil && *cfg.ResponseTimeThresholdMs < 1 {
		return errors.New("response_time_threshold_ms must be at least 1")
	}
	if cfg.MaxRedirects != nil && (*cfg.MaxRedirects < 0 || *cfg.MaxRedirects > 20) {
		return errors.New("max_redirects must be between 0 and 20")
	}
	if cfg.Auth != nil {
		switch cfg.Auth.Type {
		case "none", "basic", "bearer":
		default:
			return fmt.Errorf("auth type %q: want none, basic, or bearer", cfg.Auth.Type)
		}
	}
	switch cfg.IPFamily {
	case "", "auto", "ipv4", "ipv6":
	default:
		return fmt.Errorf("ip_family %q: want auto, ipv4, or ipv6", cfg.IPFamily)
	}
	return nil
}

// Check performs one attempt. ctx carries the monitor's timeout.
func (h *HTTP) Check(ctx context.Context, config []byte) Observation {
	var cfg httpConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return Observation{Status: model.StatusUnknown, Class: ClassConfig, Message: "config: " + err.Error()}
	}
	ranges, err := parseStatusRanges(cfg.AcceptedStatusCodes)
	if err != nil {
		return Observation{Status: model.StatusUnknown, Class: ClassConfig, Message: err.Error()}
	}

	method := cfg.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if cfg.Body != nil {
		body = strings.NewReader(*cfg.Body)
	}

	req, err := http.NewRequestWithContext(ctx, method, cfg.URL, body)
	if err != nil {
		return Observation{Status: model.StatusUnknown, Class: ClassConfig, Message: "build request: " + err.Error()}
	}
	applyHeaders(req, cfg)

	client := h.clientFor(cfg)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return classify(err, time.Since(start))
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	elapsed := time.Since(start)
	if err != nil {
		return Observation{
			Status:       model.StatusDown,
			Class:        ClassNetwork,
			ResponseTime: &elapsed,
			Code:         strconv.Itoa(resp.StatusCode),
			Message:      "reading response body: " + err.Error(),
		}
	}

	obs := Observation{
		Status:       model.StatusUp,
		ResponseTime: &elapsed,
		Code:         strconv.Itoa(resp.StatusCode),
	}

	// Assertions in the order the spec fixes — status, then keyword, then JSON
	// path, then response time — and the first failure is what the heartbeat
	// reports, so the operator sees the actual cause rather than a generic one.
	if !ranges.accepts(resp.StatusCode) {
		obs.Status = model.StatusDown
		obs.Class = ClassAssertion
		obs.Message = fmt.Sprintf("status %d is not in %s", resp.StatusCode, ranges)
		return obs
	}
	if cfg.Keyword != nil {
		if msg := assertKeyword(cfg, string(payload)); msg != "" {
			obs.Status = model.StatusDown
			obs.Class = ClassAssertion
			obs.Message = msg
			return obs
		}
	}
	if hasJSONPath(cfg) {
		assertion, err := parseJSONPathAssertion(cfg.JSONPath)
		if err != nil {
			// Validation ran at assignment time, so reaching this means the
			// config changed underneath us. Unknown, not down: nothing was
			// asserted about the target.
			obs.Status = model.StatusUnknown
			obs.Class = ClassConfig
			obs.Message = err.Error()
			return obs
		}
		if msg := assertion.assert(payload); msg != "" {
			obs.Status = model.StatusDown
			obs.Class = ClassAssertion
			obs.Message = msg
			return obs
		}
	}
	if cfg.ResponseTimeThresholdMs != nil {
		threshold := time.Duration(*cfg.ResponseTimeThresholdMs) * time.Millisecond
		if elapsed > threshold {
			obs.Status = model.StatusDown
			obs.Class = ClassAssertion
			obs.Message = fmt.Sprintf("responded in %s, over the %s threshold", elapsed.Round(time.Millisecond), threshold)
			return obs
		}
	}
	return obs
}

// hasJSONPath distinguishes an absent json_path from an explicit null, both of
// which the spec allows and neither of which is an assertion.
func hasJSONPath(cfg httpConfig) bool {
	return len(cfg.JSONPath) > 0 && string(cfg.JSONPath) != "null"
}

func (h *HTTP) clientFor(cfg httpConfig) *http.Client {
	transport := h.verifying
	if cfg.VerifyTLS != nil && !*cfg.VerifyTLS {
		transport = h.skipping
	}

	maxRedirects := 10
	if cfg.MaxRedirects != nil {
		maxRedirects = *cfg.MaxRedirects
	}
	follow := cfg.FollowRedirects == nil || *cfg.FollowRedirects

	return &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if !follow {
				return http.ErrUseLastResponse
			}
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		},
		// No client timeout: ctx carries the monitor's timeout, and two competing
		// deadlines produce two different error messages for one condition.
	}
}

func applyHeaders(req *http.Request, cfg httpConfig) {
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	if cfg.Body != nil && req.Header.Get("Content-Type") == "" {
		switch cfg.BodyEncoding {
		case "", "json":
			req.Header.Set("Content-Type", "application/json")
		case "form":
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		case "xml":
			req.Header.Set("Content-Type", "application/xml")
		case "text":
			req.Header.Set("Content-Type", "text/plain")
		}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "uptime-cairn")
	}
	if cfg.Auth != nil {
		switch cfg.Auth.Type {
		case "basic":
			req.SetBasicAuth(cfg.Auth.Username, cfg.Auth.Password)
		case "bearer":
			req.Header.Set("Authorization", "Bearer "+cfg.Auth.Token)
		}
	}
}

func assertKeyword(cfg httpConfig, body string) string {
	k := cfg.Keyword
	switch k.Mode {
	case "contains", "not_contains":
		haystack, needle := body, k.Value
		if !k.CaseSensitive {
			haystack, needle = strings.ToLower(haystack), strings.ToLower(needle)
		}
		found := strings.Contains(haystack, needle)
		if k.Mode == "contains" && !found {
			return fmt.Sprintf("keyword %q not found in response body", k.Value)
		}
		if k.Mode == "not_contains" && found {
			return fmt.Sprintf("keyword %q found in response body but should not be", k.Value)
		}
	case "regex", "not_regex":
		re, err := compileKeyword(k.Value, k.CaseSensitive)
		if err != nil {
			return "keyword regex: " + err.Error()
		}
		matched := re.MatchString(body)
		if k.Mode == "regex" && !matched {
			return fmt.Sprintf("response body does not match /%s/", k.Value)
		}
		if k.Mode == "not_regex" && matched {
			return fmt.Sprintf("response body matches /%s/ but should not", k.Value)
		}
	}
	return ""
}

func compileKeyword(pattern string, caseSensitive bool) (*regexp.Regexp, error) {
	if !caseSensitive {
		pattern = "(?i)" + pattern
	}
	return regexp.Compile(pattern)
}

// classify turns a transport error into an outcome. This function is where the
// invariant lives: a failure of the target is down, a failure of the probe is
// unknown, and reporting the second as the first is how one broken probe pages
// an entire on-call rotation.
func classify(err error, elapsed time.Duration) Observation {
	obs := Observation{Status: model.StatusDown, ResponseTime: &elapsed, Message: redact(err)}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		obs.Class = ClassTimeout
		obs.Message = "timed out after " + elapsed.Round(time.Millisecond).String()
		return obs
	case errors.Is(err, context.Canceled):
		// Shutdown, not a verdict. Saying "down" here would record an outage
		// every time the process restarts.
		obs.Status = model.StatusUnknown
		obs.Class = ClassNetwork
		obs.Message = "check cancelled"
		return obs
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		obs.Class = ClassDNS
		if dnsErr.IsNotFound {
			// The name does not exist. That is a fact about the target.
			obs.Message = "DNS: no such host " + dnsErr.Name
			return obs
		}
		// Timeout or a server failure from our own resolver. We do not know
		// whether the target is up, and saying "down" would be a guess.
		obs.Status = model.StatusUnknown
		obs.Message = "DNS lookup failed for " + dnsErr.Name + ": " + dnsErr.Err
		return obs
	}

	var tlsErr *tls.CertificateVerificationError
	if errors.As(err, &tlsErr) {
		obs.Class = ClassTLS
		return obs
	}
	if strings.Contains(err.Error(), "tls:") || strings.Contains(err.Error(), "certificate") {
		obs.Class = ClassTLS
		return obs
	}

	obs.Class = ClassNetwork
	return obs
}

// redact strips credentials from an error before it becomes a stored message. A
// URL with an embedded password escapes through error strings more often than
// through anything else, which is the specific leak ADR-005 decision 15 names.
func redact(err error) string {
	msg := err.Error()

	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if u, parseErr := url.Parse(urlErr.URL); parseErr == nil && u.User != nil {
			safe := *u
			safe.User = url.User("redacted")
			msg = strings.ReplaceAll(msg, urlErr.URL, safe.String())
		}
	}
	return msg
}
