package api

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"
)

// Custom-domain status pages.
//
// A status page carries a `custom_domain`, unique across the install. What this
// file adds is the part that makes it mean something: a request arriving on that
// hostname gets that status page, at the bare root, rather than the dashboard.
//
// # Why this is not a proxy rewrite
//
// The obvious answer is to rewrite `/` to `/status/{slug}` in nginx, and it does
// not work. The dashboard is a single-page application that reads its slug out
// of the browser's path, and an internal rewrite leaves that path as `/` — the
// router finds no slug and renders nothing. A *redirect* works and puts the slug
// in the address bar, which is a customer-visible detail on a page whose entire
// purpose is to look like the customer's own.
//
// So the resolution happens here, where the answer can reach the client: the
// application shell is served with the slug in it, and the frontend renders that
// page at whatever path it was asked for.
//
// # Why it is cached
//
// This runs on every document request, including every navigation in the
// dashboard. A query per navigation for an answer that changes when somebody
// edits a status page is the wrong trade; a short TTL is the right one. The cost
// is that a newly published custom domain takes up to one TTL to start routing,
// which is a sentence in the documentation rather than a support ticket.

// domainTTL is how long the custom-domain map is trusted.
//
// Thirty seconds: long enough that a burst of navigation costs one query, short
// enough that an operator adding a domain and reloading sees it work rather than
// concluding it is broken and changing something else.
const domainTTL = 30 * time.Second

// domainCache resolves a request's Host to a status page slug.
type domainCache struct {
	load func(context.Context) (map[string]string, error)

	mu      sync.RWMutex
	domains map[string]string
	fetched time.Time
}

func newDomainCache(load func(context.Context) (map[string]string, error)) *domainCache {
	return &domainCache{load: load}
}

// slugFor returns the status page slug this hostname serves, if any.
//
// A lookup failure returns no match rather than an error. The consequence of
// getting this wrong in the pessimistic direction is a custom domain showing the
// dashboard for thirty seconds; in the optimistic direction it would be a
// request failing outright. The first is recoverable and the second is an
// outage of the login page.
func (c *domainCache) slugFor(ctx context.Context, host string) (string, bool) {
	host = normaliseHost(host)
	if host == "" {
		return "", false
	}

	c.mu.RLock()
	domains, fresh := c.domains, time.Since(c.fetched) < domainTTL
	c.mu.RUnlock()

	if !fresh {
		loaded, err := c.load(ctx)
		if err == nil {
			c.mu.Lock()
			c.domains, c.fetched = loaded, time.Now()
			c.mu.Unlock()
			domains = loaded
		} else if domains == nil {
			// Never loaded and cannot load. No match, and the dashboard is
			// served — which is the right failure for a request that was
			// probably for the dashboard anyway.
			return "", false
		}
	}

	slug, ok := domains[host]
	return slug, ok
}

// invalidate drops the cache, so a status page write takes effect immediately
// rather than at the next TTL. Called from the status page write paths, because
// the one moment somebody is watching for this to work is right after they
// saved it.
func (c *domainCache) invalidate() {
	c.mu.Lock()
	c.fetched = time.Time{}
	c.mu.Unlock()
}

// normaliseHost strips the port and lower-cases.
//
// A Host header legitimately carries a port, and `status.acme.example` and
// `status.acme.example:443` are the same host to everyone except a map lookup.
// IPv6 literals arrive bracketed, which SplitHostPort handles and a naive
// Cut(":") does not.
func normaliseHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if stripped, _, err := net.SplitHostPort(host); err == nil {
		host = stripped
	}
	return strings.ToLower(strings.Trim(host, "[]"))
}
