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

Read this before promising a customer their own domain, because the support
today is partial and the gap is in the application rather than in your config.

A status page carries a `custom_domain`, it is unique across the install, and
subscriber mail already prefers it when building links. What does **not** exist
is host-based routing: nothing in the server resolves a request to a page by its
`Host` header. Every page is served from `/status/{slug}`, and the dashboard is a
client-side application that reads the slug out of the browser's path.

That last detail decides the recipe. An internal rewrite does not work — the
browser's path stays `/`, the router finds no slug, and the page cannot load. It
has to be a redirect, which means the slug ends up visible in the address bar:

```caddyfile
status.acme.example {
	redir / /status/acme
	reverse_proxy 127.0.0.1:3000
}
```

```nginx
server {
    listen 443 ssl http2;
    server_name status.acme.example;
    # ... TLS, headers, and the /metrics denial as above ...

    location = / { return 302 /status/acme; }
    location   / { proxy_pass http://127.0.0.1:3000; }
}
```

Point the customer's DNS at the proxy, issue a certificate for their hostname,
and the page works. `https://status.acme.example/` lands on
`https://status.acme.example/status/acme`, which is a working custom-domain
status page with a path on the end of it. Serving it at the bare root needs
`Host`-based resolution in the server, and that is not built.
