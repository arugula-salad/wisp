# API coverage

- **Sprites**: CRUD, pagination, labels, URL settings, and the `org` counts on the list response.
  Ours: `"from": {"sprite": "template", "checkpoint": "v1"}` on create starts the sprite as a
  clone of that checkpoint (newest manual one when omitted) instead of the base image, with
  the source's config and policies. With a reflink volume the clone is instant.
  Also ours: `"from": {"image": "node:22"}` starts it from a container image, pulled with
  rootless podman and cached as a disk; see [sprites from container images](images.md).
- **Exec**: WebSocket TTY/non-TTY, detach/reattach with output replay,
  `max_run_after_disconnect`, signals, session list, kill, and HTTP POST exec in upstream's
  frame format.
- **Control**: the multiplexed `/control` channel (exec, proxy and `fs.*` operations).
- **Ports**: `port_opened`/`port_closed` notifications on exec sessions (`address` is the bind
  IP, which is what the CLI dials) and `ports/watch`.
- **Checkpoints**: create/list/get/delete/restore with streaming NDJSON, `history`, and
  automatic checkpoints (`auto-<n>`, hidden unless `includeAuto`).
- **Services**: create/get/list/delete, start/stop/restart with streamed logs, signal, `needs`
  ordering, crash restart with backoff, and one `http_port` service that the sprite URL routes
  to and starts on demand (otherwise the URL goes to port 8080). Definitions live on the
  sprite's disk in `/.sprite/services/`, logs in `/.sprite/logs/services/<name>.log`, so both
  travel with checkpoints. Every service starts on a cold boot.
- **Filesystem**: read, write (atomic), list, delete, rename, copy, chmod, chown, and `watch`
  (recursive, including directories created later; bounded, and a slow reader is told how many
  events it missed rather than stalling the agent). New files belong to the `sprite` user.
- **Policies**:
  - `policy/network`: a domain allowlist with `{"include":"defaults"}` and `*.` wildcards.
    Empty rules mean unrestricted. Changes apply live. See [how network policy is enforced](security.md#how-network-policy-is-enforced).
  - `policy/privileges`: capability profiles (`minimal`, `standard`, `privileged`) and
    `noNewPrivileges`, applied to processes started after the change.
  - `policy/resources`: a memory limit, as a guest cgroup immediately and as VM RAM of
    `limit_mb + 128` from the next cold boot. `autoscale` holds the sprite to a grant below
    that, which grows under pressure; it applies live. See [memory autoscale](lifecycle.md#memory-autoscale).
  - `policy/spawn` (ours): lets the sprite create sprites of its own from inside. See
    [Sprites that create sprites](#sprites-that-create-sprites).
- **Proxy / URLs**: the TCP proxy, and per-sprite URLs with `sprite`/`public` auth (see
  [Public sprite URLs](public-urls.md) for serving them to the internet).
- **Events** (ours): a server-sent event stream at `GET /mini-sprites/v1/events` of
  lifecycle, checkpoint, service, policy, limit and disk events, with filters and
  `Last-Event-ID` resume, and signed webhooks. See [Events and webhooks](events.md).
- **Custom domains** (ours): `GET|POST /v1/sprites/{name}/domains`, `GET|DELETE
  /v1/sprites/{name}/domains/{domain}` attach names like `game.example.com` to a sprite, each
  with a certificate of its own. See [Custom domains](public-urls.md#custom-domains).

## From inside a sprite

`sprite-env` (at `/.sprite/bin`, on `$PATH`, reinstalled from the initramfs on every cold
boot) talks to `/.sprite/api.sock`, owned by the `sprite` user. It manages services and
checkpoints with no API token:

```sh
sprite-env services create web --cmd python3 --args "-m,http.server,3000" --http-port 3000
sprite-env checkpoints create && sprite-env checkpoints list
```

An old checkpoint can be browsed without restoring it:

```sh
sprite-env checkpoints mount v3          # read-only at /.sprite/checkpoints/v3
cp /.sprite/checkpoints/v3/home/sprite/app/config.yml ~/app/   # pull one file back from the past
sprite-env checkpoints unmount v3
```

Firecracker cannot hot-plug a drive but can swap the file behind one, so every VM boots with
four placeholder drives and a mount points one at the checkpoint's image: no copy, no reboot.
Mounts survive a warm suspend, are reset by a cold boot or a restore, and a mounted checkpoint
cannot be deleted. The sprite's network policy is readable at `/.sprite/policy/network.json`
(information only; enforcement is on the host).

Checkpoint calls ride a guest-initiated vsock channel to a per-VM listener bound to that one
sprite: the channel is the identity, and a guest cannot address anything but itself (and,
with a spawn policy, the sprites it created).
Restoring from inside ends the session, since the VM is replaced.

## Sprites that create sprites

Upstream, an app inside a sprite that wants more sprites carries an API token and calls the
public API. Here a guest cannot reach the host at all, and the one token is the whole API, so
spawning rides the same in-guest socket as checkpoints, and is off until you grant it:

```sh
# from outside, once
curl -X POST $SPRITES_API_URL/v1/sprites/lobby/policy/spawn -H "Authorization: Bearer $TOKEN" \
  -d '{"enabled": true, "max_children": 20, "sources": ["game-template"]}'

# from inside "lobby", no token
sprite-env sprites create game-42 --from game-template --public    # prints the sprite, url included
sprite-env sprites create tool-1 --image node:22                    # an image already in the host's cache
sprite-env sprites list
sprite-env sprites delete game-42
# or: curl --unix-socket /.sprite/api.sock http://sprite/v1/sprites -d '{"name":"game-42","from":{"sprite":"game-template"},"url_settings":{"auth":"public"}}'
```

The pattern this is for: set a template sprite up once (install the app, define its
`http_port` service, `sprite-env checkpoints create`), then have a front sprite clone it per
visitor and redirect to the new sprite's `url`. Services travel with the disk and the URL
starts the sprite on demand, so nothing has to be run in the clone: create to first HTTP
response measured 475 ms, without reflinks.

What a spawner can and cannot do:

- It sees and deletes only the sprites it created (`parent_id` on the sprite); everything else is 404.
- It holds at most `max_children` of them (default 10); `--max-sprites` and the disk guard still apply.
- It may clone its own checkpoints, its children's, and those of the sprites in `sources`.
- It may start a child from a container image only if the image is already in the host's
  cache (`spritesd images pull`); a guest can never make the host pull. See [images](images.md).
- A child always runs under the spawner's network policy, so spawning is no way out of one. It
  gets the spawner's config and other policies, or the source's when it is a clone; `config`
  in the request is ignored. It may choose `public` URL auth, environment and labels.
- A child has no spawn policy of its own. Exec, the filesystem API and policies of a child are
  not reachable from inside: put what a child needs in the template.
- It can follow its children's events, and only theirs: `sprite-env sprites events` (see
  [Events and webhooks](events.md#from-inside-a-sprite)).
- Deleting a spawner leaves its children in place.
- Sprites running an agent from before this feature get the routes on their next cold boot.
