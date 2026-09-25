# Public sprite URLs

Every sprite has a URL, `<name>.<url-domain>`, that wakes it and proxies to its `http_port`
service (else port 8080). Out of the box that is `http://<name>.sprites.localhost:7788`, on
the same listener as the API. To put the URLs on the internet without putting the API there:

```sh
# once, in Cloudflare: an A record  *.widgets.wtf -> your public IP  (DNS only, grey cloud),
# and an API token with Zone:Read + DNS:Edit on that zone
(umask 077; echo "$TOKEN" > ~/.local/share/wisp/cloudflare-token)
# once, on the router: forward external 443 -> this machine's 8443

./bin/wispd --url-domain widgets.wtf --public-listen :8443
```

- `--public-listen` serves sprite URLs over HTTPS and **nothing else**: the management API has
  no routes there, so a leaked forward cannot expose it. Keep `--listen` on loopback or a tailnet.
- The certificate is one wildcard, `*.widgets.wtf`, from Let's Encrypt over DNS-01, renewed at
  two thirds of its life and kept in `<data>/acme/`. A wildcard because sprite names then never
  appear in certificate-transparency logs, a new sprite's URL works at once, and DNS-01 needs no
  inbound port. Try `--acme-directory https://acme-staging-v02.api.letsencrypt.org/directory`
  first: production rate-limits failed attempts. No Cloudflare? Bring any certificate with
  `--tls-cert/--tls-key`; the files are re-read when they change.
- A sprite's URL needs the API token as a bearer unless its `url_settings.auth` is `public`,
  which is upstream's model and the default is the closed one. Anyone can wake a `public`
  sprite, and it holds its RAM until it idles out again.
- **A visitor waits for the app, briefly.** A request to a sleeping sprite wakes it, and the VM
  is back long before the app inside has bound its port; the visitor used to get a proxy error
  on a sprite that was about to work. The URL proxy now retries the guest port for up to 10
  seconds (`--url-ready-wait`, a Go duration; `0` fails on the first refused
  connection as before, and 60s is the hard ceiling whatever is set) before answering `503`
  with `Retry-After: 5` and a body saying the sprite is starting — honest and temporary, where
  the old `502` said the site was broken. The wait is bounded and lives inside the request: it
  runs under the keep-awake hold the request already has, a visitor who gives up ends it, and
  it never keeps an idle sprite up on its own. Only sprite URLs are gated; the API's own proxy
  and exec paths are unchanged. If the sprite's `http_port` service owns the port, the agent
  also waits for it inside the guest, so the two waits overlap rather than add up.
- The API reports `https://<name>.widgets.wtf`; add `--public-port` if the router's outside
  port is not 443. Connections are capped in total and per client (`--public-max-conns*`).
- Proxying through Cloudflare (orange cloud) also works and hides your address: use an outside
  port Cloudflare connects to (443, 8443, 2053...), SSL mode "Full (strict)", and
  `--public-max-conns-per-client 0`, since every visitor then arrives from Cloudflare's addresses.
- Outside 443 already taken by another reverse proxy? Have it pass the TLS through by SNI rather
  than terminate it, so the certificate and the per-sprite auth stay here. In Traefik that is an
  `IngressRouteTCP` on the HTTPS entrypoint matching ``HostSNIRegexp(`^[a-z0-9-]+\.widgets\.wtf$`)``
  with `tls.passthrough: true`, pointing at this machine's 8443. It also needs
  `--public-max-conns-per-client 0`: every visitor arrives from the proxy's address.
- Not handled: updating the A record when a dynamic IP changes, and a port-80 redirect (a
  reverse proxy in front can do the redirect).

## Several URL domains

`--url-domain` takes a comma-separated list, such as `widgets.wtf,arugula.io`. The first is
the default. Each sprite is under exactly one of them:

- `POST /v1/sprites` takes `"url_domain"`, which must be one of the list; without it the
  sprite gets the first. A sprite made from inside (a spawner's child) always gets its
  parent's, whatever it asks for, so a studio on one domain makes its apps on that domain.
- The API reports it as `url_domain`, next to `url`. A sprite answers only under its own
  domain: `app-1.arugula.io` is not `app-1.widgets.wtf`'s URL.
- Each domain gets its own wildcard certificate (DNS-01 through the same Cloudflare token,
  which needs the extra zone). The first stays in `<data>/acme/<ca>/`; the others are in
  `<data>/acme/<ca>/url-domains/<domain>/`, and the listener picks one by SNI.
  `--tls-cert/--tls-key` cover a single domain only.
- One domain may be nested in another, e.g. `arugula.io,games.arugula.io`. A host belongs to
  the most specific domain it is strictly under: `x.games.arugula.io` is sprite `x` under
  `games.arugula.io`, and gets that domain's certificate; `games.arugula.io` itself is still
  sprite `games` under `arugula.io`. Each needs its own wildcard record, since `*.arugula.io`
  does not cover `x.games.arugula.io`.
- Every domain needs its own wildcard DNS record (and passthrough, behind a proxy), exactly
  like the first. Sprites made before there were several have no `url_domain` stored and are
  under the first, so put the domain you already use first.

## Custom domains

Ours, not upstream's. A sprite can also answer at names you choose, such as
`game.example.com`, each with a certificate of its own, on the same `--public-listen`
listener and under the same `url_settings.auth` as its `<name>.<url-domain>` URL:

```sh
# at your DNS provider: game.example.com  CNAME  game.widgets.wtf
curl -X POST $SPRITES_API_URL/v1/sprites/game/domains -H "Authorization: Bearer $TOKEN" \
  -d '{"domain": "game.example.com"}'
# {"domain":"game.example.com","status":"pending","reason":"not checked yet",...}
curl $SPRITES_API_URL/v1/sprites/game/domains -H "Authorization: Bearer $TOKEN"
# {"domains":[{"domain":"game.example.com","status":"issued","not_after":"...","next_attempt":"..."}]}
curl -X DELETE $SPRITES_API_URL/v1/sprites/game/domains/game.example.com -H "Authorization: Bearer $TOKEN"
```

- **Status** is `pending` (waiting for DNS, for the order budget or for the CA; `reason` says
  which), `issued` (a current certificate is served; during a renewal `reason` says what the
  renewal is waiting on) or `error` (the CA refused; `reason` has its message and
  `next_attempt` when it is tried again). `inactive` means the daemon runs without
  `--public-listen`. The web UI shows the same on the sprite's overview.
- **Ownership check.** Nothing is asked of the CA until the domain's DNS leads here: every A and
  AAAA address the domain resolves to (CNAMEs followed) must be one that `<name>.<url-domain>`
  resolves to, and there must be at least one. So a CNAME to the sprite's own URL passes, as
  does an A record to the same address as the wildcard record; a domain that points elsewhere,
  even partly, stays `pending`. The lookup goes to `--domain-resolver` (default `1.1.1.1:53`, a
  public resolver, because a home resolver may give a split-horizon answer). A domain proxied
  by Cloudflare (orange cloud) resolves to Cloudflare's addresses and does not pass unless the
  wildcard is proxied too, and even then TLS-ALPN-01 cannot pass a proxy that terminates TLS.
- **Certificates** come from `--acme-directory` over TLS-ALPN-01, which the CA runs by
  connecting to port 443 of the domain with the `acme-tls/1` ALPN protocol; the public
  listener answers it in the handshake. That is the one challenge that works here: DNS-01
  would need write access to the domain's DNS, and HTTP-01 needs port 80. They are kept in
  `<data>/acme/<ca>/domains/<domain>/`, renewed at two thirds of their life, served by SNI,
  and deleted on detach. The ACME account is the wildcard's. `--tls-cert/--tls-key` only
  replace the wildcard; custom domains still use ACME.
- **Limits.** A domain belongs to one sprite (a second attach is `409 domain_taken`);
  `--max-domains-per-sprite` (5) and `--max-domains` (50) cap attachments (`403
  domain_limit_exceeded`). Let's Encrypt allows 5 failed validations per hostname per hour,
  so a failing domain backs off (DNS checks from 1 minute doubling to 30, CA failures from 5
  minutes doubling to 6 hours), and `--acme-orders-per-hour` (10) caps orders across all
  domains. A failed DNS check costs no order. `--custom-domains=false` turns the feature off.
- Deleting a sprite detaches its domains. A clone (`"from"`) starts with none. A sprite
  restored from a backup keeps the domains no other sprite has taken meanwhile. Names under
  `--url-domain`, IP addresses, wildcards and `localhost` are refused; write an IDN in its
  `xn--` form.
- An unknown name is treated as before: it gets the wildcard certificate and a 404.
- Not covered: CAA records (the CA checks them; one that excludes Let's Encrypt fails with the
  CA's reason), apex domains at DNS providers that cannot flatten a CNAME (use A/AAAA records
  equal to the wildcard's), and a status history across restarts (errors are kept in memory;
  a restart retries at once, certificates on disk are served straight away).

### Behind an SNI-routing proxy

Every custom domain has to reach the public listener on port 443 with its TLS untouched. The
Traefik route above matches only ``HostSNIRegexp(`^[a-z0-9-]+\.widgets\.wtf$`)``, so a custom
domain arriving at Traefik is handed to Traefik's own HTTP routers instead and gets its
default certificate; the CA's TLS-ALPN-01 probe fails the same way. The operator adds each
custom domain to the passthrough route, for example in the `IngressRouteTCP` `sprite-urls`:

```yaml
  routes:
    - match: HostSNIRegexp(`^[a-z0-9-]+\.widgets\.wtf$`) || HostSNI(`game.example.com`)
      services:
        - name: wispd-public
          port: 8443
```

or as a separate `IngressRouteTCP` per domain on the same entrypoint and service, with
`tls.passthrough: true`. Do **not** use a catch-all ``HostSNI(`*`)``: Traefik matches TCP
routers before HTTP routers on an entrypoint, so a catch-all would take every other HTTPS
site on that entrypoint as well. If Traefik has a certificate resolver that itself uses the
TLS challenge, it answers `acme-tls/1` handshakes itself; set
`--entryPoints.websecure.allowACMEByPass=true` so they reach the passthrough route (without
such a resolver Traefik already passes them through). Port 80 needs nothing: TLS-ALPN-01
never uses it, though a redirect for the custom domain on the `web` entrypoint is a courtesy.
The route change has to land before the attach, or the first orders fail and back off.
