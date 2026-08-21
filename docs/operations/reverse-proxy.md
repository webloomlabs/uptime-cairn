# Reverse-proxy recipes

The binary speaks plain HTTP and has no TLS flags — there is no `--tls-cert`,
and that is deliberate rather than unfinished. Terminating TLS is a solved
problem with better implementations than one we would write, and a monitoring
tool that ships its own certificate handling is a monitoring tool that ships its
own certificate bugs. So: bind it to loopback, put a proxy in front.

```sh
cairn --data-dir /var/lib/uptime-cairn --listen 127.0.0.1:3000 \
      --base-url https://status.example.com
```

`--base-url` is not cosmetic. It is what alerts and subscriber mail put in their
links, and it must be the address the proxy answers on, not the one the process
binds to.

## Two things every recipe below has to do

### Block `/metrics` at the proxy

`/metrics` requires an API key with `metrics:read` — **except from loopback,
where it is served unauthenticated**, so that a local Prometheus needs no
credential. The check is on the connection's remote address, and behind a
same-host proxy every connection arrives from `127.0.0.1`. The exemption meant
for a scraper on the box therefore applies to the entire internet.

Every recipe here denies `/metrics` at the proxy for that reason. If you scrape
from another host, allow it through and require the key
(`Authorization: Bearer …` with `metrics:read`); if you scrape locally, go
straight to `127.0.0.1:3000` and bypass the proxy.

### Know that `X-Forwarded-For` is not read

The server keys its login rate limiter on the connection's remote address —
`X-Forwarded-For` is caller-controlled unless a trusted proxy is configured, and
trusting it would hand an attacker an unlimited supply of rate-limit buckets.
Behind a proxy that address is constant, so the limiter degenerates to five
failed attempts per *account* per fifteen minutes rather than per account per
source. That is still a working limiter and it is still per account; it just
stops distinguishing where the guesses came from. Set the headers anyway — they
cost nothing and the day the server does consume them, your config is already
right.

## Caddy

The short one, because Caddy gets certificates on its own:

```caddyfile
status.example.com {
	# Denied before the reverse_proxy directive can see it.
	respond /metrics 404

	reverse_proxy 127.0.0.1:3000

	header {
		Strict-Transport-Security "max-age=31536000; includeSubDomains"
		X-Content-Type-Options "nosniff"
		X-Frame-Options "DENY"
		Referrer-Policy "strict-origin-when-cross-origin"
	}
}
```

The server sets none of those headers itself, which is why they are here. That
is a gap in the application rather than a division of labour — it is Phase 1
security work that has not landed — and setting them at the edge is correct in
the meantime.

## nginx

```nginx
server {
    listen 443 ssl http2;
    server_name status.example.com;

    ssl_certificate     /etc/letsencrypt/live/status.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/status.example.com/privkey.pem;

    add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;
    add_header X-Content-Type-Options "nosniff" always;
    add_header X-Frame-Options "DENY" always;
    add_header Referrer-Policy "strict-origin-when-cross-origin" always;

    # See "Block /metrics at the proxy" above. This is not optional.
    location = /metrics { return 404; }

    location / {
        proxy_pass http://127.0.0.1:3000;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # The dashboard polls; it does not hold a stream open. There is no
        # WebSocket or SSE endpoint in this build, so no upgrade handling is
        # needed here — if you have carried an `Upgrade` block over from another
        # tool's config, it is inert.
        proxy_read_timeout 60s;
    }
}

server {
    listen 80;
    server_name status.example.com;
    return 301 https://$host$request_uri;
}
```

## Traefik

Labels, for the compose file:

```yaml
services:
  cairn:
    image: ghcr.io/webloomlabs/uptime-cairn:latest
    volumes:
      - cairn-data:/data
    labels:
      - "traefik.enable=true"
      - "traefik.http.routers.cairn.rule=Host(`status.example.com`)"
      - "traefik.http.routers.cairn.entrypoints=websecure"
      - "traefik.http.routers.cairn.tls.certresolver=letsencrypt"
      - "traefik.http.services.cairn.loadbalancer.server.port=3000"
      - "traefik.http.services.cairn.loadbalancer.healthcheck.path=/healthz"

      # Higher priority than the catch-all router above, so it wins the match.
      - "traefik.http.routers.cairn-metrics.rule=Host(`status.example.com`) && Path(`/metrics`)"
      - "traefik.http.routers.cairn-metrics.entrypoints=websecure"
      - "traefik.http.routers.cairn-metrics.priority=100"
      - "traefik.http.routers.cairn-metrics.tls.certresolver=letsencrypt"
      - "traefik.http.routers.cairn-metrics.service=noop@internal"
      - "traefik.http.routers.cairn-metrics.middlewares=cairn-deny"

      - "traefik.http.middlewares.cairn-deny.ipallowlist.sourcerange=127.0.0.1/32"
      - "traefik.http.middlewares.cairn-sec.headers.stsSeconds=31536000"
      - "traefik.http.middlewares.cairn-sec.headers.contentTypeNosniff=true"
      - "traefik.http.middlewares.cairn-sec.headers.frameDeny=true"
      - "traefik.http.routers.cairn.middlewares=cairn-sec"
```

Drop `127.0.0.1:3000:3000` from the service when Traefik is on the same Docker
network — the port does not need publishing for Traefik to reach it, and not
publishing it is one fewer way to reach the API without TLS.

## Custom-domain status pages

A status page can answer on a customer's own hostname —
`https://status.acme.example/` — showing their page at the bare root, with no
`/status/acme` in the address bar.

That works because the *server* resolves it, not the proxy. A request arriving
with a `Host` matching a page's `custom_domain` is served the application shell
with that page's slug in it, and the frontend renders it. The proxy's job is
what it always is: terminate TLS and pass the request through with the original
`Host` intact.

**An internal rewrite in the proxy does not work and never will.** The dashboard
reads its slug out of the browser's path, and a rewrite leaves that path as `/`.
This was previously documented as a redirect to `/status/{slug}` for that
reason; that workaround is no longer needed, and if you have one configured you
can remove it.

### What the proxy has to do

Exactly two things, and both are defaults in Caddy and Traefik:

1. **Pass the original `Host` through.** This is the whole mechanism. nginx does
   *not* do it by default — `proxy_set_header Host $host;` is required, and
   without it every custom domain shows the dashboard.
2. **Hold a certificate for the hostname.**

### Caddy

```caddyfile
status.acme.example {
	reverse_proxy 127.0.0.1:3000
}
```

Caddy passes `Host` through and obtains the certificate itself. That is the
whole configuration.

**For many customer domains, use on-demand TLS** rather than listing each one:

```caddyfile
{
	on_demand_tls {
		# Caddy asks this before issuing. Answer 200 to allow, anything else to
		# refuse. Without an ask endpoint, anyone who points DNS at your server
		# can make you request a certificate for their hostname — which is a
		# rate-limit exhaustion attack against your Let's Encrypt account, not a
		# theoretical one.
		ask http://127.0.0.1:3000/api/v1/public/status-pages/domain-check
		interval 2m
		burst 5
	}
}

https:// {
	tls {
		on_demand
	}
	reverse_proxy 127.0.0.1:3000
}
```

> **The `ask` endpoint does not exist yet.** Caddy calls it with `?domain=`, and
> Uptime Cairn has no endpoint that answers "is this a configured custom
> domain". Until it does, either list your domains explicitly in the Caddyfile,
> or point `ask` at a two-line script of your own that greps your own list. Do
> not run on-demand TLS without an ask endpoint: it is the difference between
> issuing certificates for domains you configured and issuing them for anything
> anyone points at you.

### nginx

`Host` is the thing to get right:

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name status.acme.example;

    ssl_certificate     /etc/letsencrypt/live/status.acme.example/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/status.acme.example/privkey.pem;

    # Without this nginx sends `Host: 127.0.0.1:3000` and the custom domain
    # resolves to nothing. This is the single most common way this feature
    # appears broken.
    proxy_set_header Host $host;

    proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;

    location / {
        proxy_pass http://127.0.0.1:3000;
    }

    # A status page is public; /metrics is not, and it is reachable through any
    # server block that proxies everything.
    location /metrics { return 404; }
}
```

Certificates with certbot, one hostname at a time:

```sh
sudo certbot certonly --nginx -d status.acme.example
```

Or, for a lot of them, `acme.sh` in a loop against a list you keep — nginx has
no on-demand equivalent, so issuance is something you run rather than something
that happens.

### Traefik

Traefik passes `Host` through and resolves certificates per router:

```yaml
labels:
  - "traefik.enable=true"
  - "traefik.http.routers.cairn.rule=Host(`cairn.example.com`) || Host(`status.acme.example`)"
  - "traefik.http.routers.cairn.tls.certresolver=le"
  - "traefik.http.services.cairn.loadbalancer.server.port=3000"
  # /metrics is denied at the edge — see above.
  - "traefik.http.routers.cairn-metrics.rule=PathPrefix(`/metrics`)"
  - "traefik.http.routers.cairn-metrics.priority=100"
  - "traefik.http.routers.cairn-metrics.middlewares=deny@file"
```

Adding a customer means adding a `Host()` to the rule and reloading. Traefik has
no on-demand issuance either.

### Setting it up, end to end

1. **Create the status page** and set its custom domain in the editor. It has to
   be **published** — an unpublished page is a draft, and a draft answering on a
   customer's hostname is the one thing you must not get by accident.
2. **Point the customer's DNS** at your proxy. A `CNAME` to your own hostname is
   easier for them to maintain than an `A` record to your IP.
3. **Issue the certificate.** Caddy does it; nginx and Traefik need telling.
4. **Load `https://status.acme.example/`.**

A newly published domain starts routing within thirty seconds — the hostname map
is cached with that TTL, and it is dropped immediately on any status page write,
so saving from the dashboard takes effect at once. Adding one directly in the
database waits for the TTL.

### The failure modes, in the order you will hit them

| Symptom | Cause |
|---|---|
| The dashboard appears on the customer's domain | The proxy is not passing `Host` through. On nginx, `proxy_set_header Host $host;`. |
| A certificate error | No certificate for that hostname yet. Caddy: check the ask endpoint is not refusing. |
| The page loads but says not found | The slug is right and the page is **not published**. |
| It worked and then stopped | The `custom_domain` was changed or cleared on the page. It is unique across the install, so it may have moved to another page. |

### One domain, one page

`custom_domain` is unique across every status page and across every
organisation, enforced by the schema rather than by the handler. Two pages
cannot claim one hostname, because a request arrives with nothing but a `Host`
header to route on and there would be no way to choose.
