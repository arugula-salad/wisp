# API coverage

- **Sprites**: CRUD, pagination, labels, URL settings, and the `org` counts on the list response.
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
    `limit_mb + 128` from the next cold boot.
- **Proxy / URLs**: the TCP proxy, and per-sprite URLs with `sprite`/`public` auth (see
  [Public sprite URLs](public-urls.md) for serving them to the internet).

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
sprite: the channel is the identity, and a guest cannot address anything but itself.
Restoring from inside ends the session, since the VM is replaced.
