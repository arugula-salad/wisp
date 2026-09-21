# Public sprite URLs

Every sprite has a URL, `<name>.<url-domain>`, that wakes it and proxies to its `http_port`
service (else port 8080). Out of the box that is `http://<name>.sprites.localhost:7788`, on
the same listener as the API. To put the URLs on the internet without putting the API there:

```sh
# once, in Cloudflare: an A record  *.widgets.wtf -> your public IP  (DNS only, grey cloud),
# and an API token with Zone:Read + DNS:Edit on that zone
(umask 077; echo "$TOKEN" > ~/.local/share/mini-sprites/cloudflare-token)
# once, on the router: forward external 443 -> this machine's 8443

./bin/spritesd --url-domain widgets.wtf --public-listen :8443
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
