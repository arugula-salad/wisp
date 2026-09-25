# API keys and serving the API in public

Every request to the API carries `Authorization: Bearer <token>`. Two kinds of token work:

- the **root token** in `<data>/token`, written on first start. It is the break-glass key:
  always admin, never revoked except by replacing the file and restarting;
- **API keys**, made and revoked one by one, each with a name and a scope.

Hand out keys, not the root token: an app, a CI job or a person each get their own, and losing
one means revoking that one.

## Scopes

| scope | may |
|---|---|
| `admin` | everything the root token can: the whole API, private sprite URLs (`url_settings.auth: "sprite"`), the dashboard |
| `read` | `GET` and `HEAD` only, and no WebSockets: list and inspect sprites, checkpoints, policies, the event stream, and read a sprite's files. Not exec, the TCP proxy, `/control`, or any change. Private sprite URLs refuse it |

A read key that tries more gets `403 {"error":"forbidden"}`; a missing or unknown key gets
`401` with `WWW-Authenticate: Bearer realm="wisp"`.

## Managing keys

On the host, over the operator socket (so it works whatever has leaked, and needs a running
daemon):

```sh
$ wispd keys create arcade-lobby                  # admin by default
created admin key "arcade-lobby" (id 3f9c21ab). This is the only time it is shown:
wisp_3f9c21ab_5d0e…
$ wispd keys create grafana --scope read
$ wispd keys list
ID        NAME          SCOPE  CREATED           LAST USED
3f9c21ab  arcade-lobby  admin  2026-09-25 13:25  2026-09-25 13:40
$ wispd keys revoke arcade-lobby                  # by name or id
```

The key goes to stdout and the message to stderr, so `KEY=$(wispd keys create ci)` works. Or
from the dashboard's **Keys** page, when signed in with an admin key or the root token. The
bearer API itself cannot make or revoke keys, so one leaked key cannot mint another.

A key reads `wisp_<id>_<secret>`. Only a SHA-256 of it is kept, in `<data>/keys.json`
(mode 0600); the id is what lists and logs show. `last_used_at` is written at most once a
minute per key. If `keys.json` cannot be read, the daemon logs it, no key works and none can
be made (so the file is not overwritten), and the root token still does.

Revoking a key refuses its next request and signs out the dashboard sessions made with it.
A connection already open with it, such as an exec session or the event stream, carries on
until it closes.

## Serving the API in public

The API listener stays on `127.0.0.1:7788` (or a tailnet address). To serve it in public, put a
TLS-terminating reverse proxy in front and tell wispd the names it serves the API under:

```sh
wispd --api-host wisp.widgets.wtf ...
```

An `--api-host` name is the bearer API and nothing else:

- never a sprite, even when it sits under a `--url-domain` (`wisp.widgets.wtf` under
  `widgets.wtf` is not the sprite `wisp`);
- never the dashboard: `/` and `/ui/` answer 401 there, and the dashboard's cookie counts for
  nothing on that name. The dashboard stays on the names the proxy does not serve: localhost, a
  tailnet address, an SSH tunnel.

The proxy must pass the `Host` header through unchanged, and the name must be kept out of any
SNI passthrough route for the wildcard, so the proxy terminates its TLS rather than handing it
to the public listener. With Traefik (as in [public URLs](public-urls.md)), that is
`&& !HostSNI(`wisp.widgets.wtf`)` on the passthrough match, plus an ordinary `IngressRoute`
for `Host(`wisp.widgets.wtf`)` to the API listener with its own certificate.

Clients then use `SPRITES_API_URL=https://wisp.widgets.wtf` and a key as the token.

## Not built

Keys scoped to particular sprites, expiry, cutting off open connections at revocation, and
throttling failed attempts per client (keys carry 256 random bits, so guessing is not the
risk; behind a proxy every request also arrives from the proxy's address). See item 5 of the
[feature brief](feature-brief.md).
