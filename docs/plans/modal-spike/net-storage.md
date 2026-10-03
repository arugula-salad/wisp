# Modal on wisp: networking, tunnels, secrets, volumes, mounts, snapshots, going public

Read-only research for the spike. Client: `modal` 1.6.0. Paths written `modal/...` are under
the site-packages of a venv with `modal==1.6.0` installed. Wisp paths are relative to the repo root.
Proto fields were dumped from the wheel's descriptors (`modal_proto/api_pb2.py`,
`task_command_router_pb2.py`), not guessed.

## Summary

- **The current spike ignores `block_network`/allowlists without saying so.** `frontend/modal/control.go`
  `create()` (lines 233-250) refuses ports, secrets, volumes, tags and names, but never reads
  `def.NetworkAccess`. `Sandbox.create(block_network=True)` therefore gets a sandbox with a network
  whenever the host has one. Fix this first: refuse the setting or enforce it. Enforcing it is cheap (below).
- **Ports/tunnels.** HTTPS tunnels map cleanly onto the Daytona preview pattern (Host-routed reverse proxy over
  `engine.DialPort`) under `*.modal.sandbox.inevitable.fyi`. Cost: **S-M**. Raw TCP (`unencrypted_ports`)
  can't go through Traefik's HTTP routers. It needs a host port range that sandboxd listens on itself, or a
  refusal. `modal.forward()`/`TunnelStart` only works in a Modal *container* (Functions), so it is out of scope.
- **Network controls.** `block_network` and `outbound_domain_allowlist` map onto `store.Record.NetworkRules`
  and `internal/netpolicy` almost directly (**S**). Runtime `TaskSetNetworkAccess` maps onto
  `Engine.SetNetworkPolicy`, which already closes connections the new policy no longer allows, as Modal
  documents. `outbound_cidr_allowlist` needs a CIDR extension to netpolicy (**M**). The experimental
  `outbound_policy` (header injection) needs a TLS MITM proxy, so refuse it (**L**).
- **Secrets.**
  - V2 never sends `Secret.from_dict/from_dotenv/from_local_environ` to the server as secrets. The client
    inlines their values into `SandboxCreateV2Request.ephemeral_secrets` and `TaskExecStartRequest.env`,
    which already works.
  - Only `Secret.from_name` needs a server store: `SecretGetOrCreate/GetInfo/List/Delete/Update`, with
    `secret_ids` on create and exec. **S-M.**
  - V1 `env=` turns into `SecretGetOrCreate(ANONYMOUS_OWNED_BY_APP)`, which is unimplemented today, so V1
    `env=` is probably broken. UNVERIFIED by running it.
- **Volumes.** Use v2 only. The server picks the version (`metadata.version`), so answering V2 keeps the client
  off the v1 S3/ETag/multipart path.
  - Data moves as plain HTTP `PUT`/`GET` to URLs the server hands out: 8 MiB blocks keyed by sha256, an
    opaque `put_response`, an optional `Repr-Digest`. sandboxd can serve those URLs itself.
  - Getting volumes into a sandbox is the hard part. The guest kernel has NFS and overlayfs but no FUSE,
    9p or virtio-fs. The recommendation is copy-in at boot and copy-out on commit or terminate, through the
    agent (**M-L**).
  - CloudBucketMount (needs FUSE) and NetworkFileSystem (deprecated, forces V1) are out of scope.
  - Dict and Queue are not needed by sandboxes.
- **Snapshots.**
  - `snapshot_filesystem` (router `TaskSnapshotFilesystem`, for both V1 and V2) is wisp's
    `Engine.CreateCheckpoint` (pause plus reflink). The result is exported into a Modal image store that
    `SandboxCreate` can boot from (`CreateSpec.ImageDisk`). **M**, which includes generalizing the
    image-id-to-disk map and `ImageFromId`.
  - `snapshot_directory`/`mount_image` can reuse the 4 read-only checkpoint drive slots plus overlayfs
    (**M-L**), or copy-in (**M**).
  - The exit snapshot is **S** once fs snapshots exist.
  - Memory snapshots that restore into a *new* sandbox need cloning a warm snapshot under a new IP and vsock
    (**L**). Defer them.
- **Going public.**
  - Add `--modal-public-url https://modal.sandbox.inevitable.fyi`. It sets the router URL (which must be
    https for a non-localhost client), the tunnel domain, and the volume block URL base.
  - Traefik: `h2c://` backend for gRPC; a separate `http://` (HTTP/1.1) service for tunnels so websockets
    work; wildcard DNS and a DNS-01 wildcard cert; `readTimeout` raised.
  - sandboxd: add `/healthz` and `grpc.health.v1`. Modal is the only front end without a `/healthz`, even
    though commit c22e990's message says otherwise. Raise grpc-go's 4 MiB `MaxRecvMsgSize`.

## Table

| # | Feature | Client path | RPCs | Wisp mapping | Effort | Main risk |
|---|---|---|---|---|---|---|
| 1a | `encrypted_ports`, `h2_ports` + `sb.tunnels()` | `modal/sandbox.py:1070-1074` (V2 create), `:540-546` (V1), `:1188-1195` (create-time cache), `:1928-1965` (`tunnels()`), `modal/_tunnel.py:18-58` (`Tunnel`) | `SandboxCreateV2Response.tunnels`, `SandboxGetTunnels[V2]` | Host-routed reverse proxy like `frontend/daytona/preview.go`, dialing via `engine.DialPort` (`engine/portdial.go:27`) | S-M | TLS-over-raw-TCP (`tls_socket`) needs SNI passthrough |
| 1b | `unencrypted_ports` (raw TCP) | same, `PortSpec.unencrypted=true`; `Tunnel.tcp_socket` (`_tunnel.py:51`) | same; `TunnelData.unencrypted_host/port` | sandboxd listens on an allocated host port and splices to `DialPort` | M | Traefik HTTP can't route it; needs a firewall port range, may be blocked by the hosting setup |
| 1c | `modal.forward()` | `_tunnel.py:186` requires `CLIENT_TYPE_CONTAINER` | `TunnelStart/TunnelStop` | n/a (in-container client only) | out of scope | — |
| 1d | `create_connect_token` | `sandbox.py:1967-2003` | `SandboxCreateConnectToken[V2]` returns `{url, token}` | Signed token; same proxy as 1a, with the token checked | S-M | Header and metadata conventions UNVERIFIED |
| 1e | `inbound_cidr_allowlist` | `sandbox.py:1152` | `Sandbox.inbound_cidr_allowlist` | Checked in the tunnel proxy against the client IP (X-Forwarded-For from Traefik) | S | Trusting XFF only from Traefik |
| 2a | `block_network` | `sandbox.py:1076-1085` | `Sandbox.network_access{BLOCKED}` | `NetworkRules=[{Domain:"*",Action:"deny"}]`, or no NIC | S | Currently ignored silently |
| 2b | `outbound_domain_allowlist` | `sandbox.py:1091-1095` | `network_access{ALLOWLIST, allowed_domains}` | `store.NetworkRule` allow rules + netpolicy (`internal/netpolicy/rules.go`) | S | `*.x` apex semantics may differ |
| 2c | `outbound_cidr_allowlist` (`cidr_allowlist` deprecated) | same | `allowed_cidrs` | New: prefixes in `netpolicy.Policy`, checked in `Enforcer.authorize` (`enforcer.go:178`) | M | DNS must stay open for CIDR-only policies; `Blocked()` still refuses private ranges |
| 2d | `_experimental_set_outbound_network_policy` | `sandbox.py:1533-1563` | router `TaskSetNetworkAccess` | `Engine.SetNetworkPolicy` (`engine/egress.go:310`) | S (after 2b/2c) | — |
| 2e | `_experimental_outbound_policy` / `_experimental_update_outbound_policy` | `modal/_outbound_policy.py`, `sandbox.py:1565-1591` | `Sandbox.outbound_policy`, router `TaskSetOutboundPolicy` | None: wisp's proxy is an L4 transparent proxy (`netpolicy/proxy.go`) | L, refuse | Needs a TLS MITM and a guest trust store |
| 2f | `i6pn`, `proxy=`, `custom_domain` | `sandbox.py:1145-1160` | `i6pn_enabled`, `proxy_id`, `custom_domain` | none | refuse | — |
| 3a | `Secret.from_dict/from_dotenv/from_local_environ` | `secret.py:298-430`; V2 inlines them via `_local_secret_env` (`secret.py:594`, `sandbox.py:1120,1168`); exec inlines them (`sandbox.py:2316-2324`) | none on V2. V1: `SecretGetOrCreate(ANONYMOUS_OWNED_BY_APP, env_dict, app_id)` (`secret.py:216-233`, `sandbox.py:899-901`) | V2 works now (`control.go:199`). V1 needs `SecretGetOrCreate` | S | V1 `env=` likely broken today |
| 3b | `Secret.from_name`, `.objects.create/list/delete`, `.update`, `.info` | `secret.py:31-213,439-487,554-583` | `SecretGetOrCreate`, `SecretGetInfo`, `SecretList`, `SecretDelete`, `SecretUpdate`; `Sandbox.secret_ids`, `TaskExecStartRequest.secret_ids` | New secret store in `<data>/modal/` (0600, optionally encrypted) | S-M | Secrets in argv and `Record.Ext` today |
| 4a | `Volume.from_name/ephemeral/from_id/objects.*` | `volume.py:277-470,708-870` | `VolumeGetOrCreate`, `VolumeGetById`, `VolumeList`, `VolumeDelete`, `VolumeRename`, `VolumeHeartbeat` | New volume registry | S | — |
| 4b | Volume file API (v2) | `volume.py:933-1210,1472-1700` | `VolumePutFiles2`, `VolumeGetFile2`, `VolumeListFiles2`, `VolumeRemoveFile2`, `VolumeCopyFiles2` + HTTP PUT/GET of block URLs | Content-addressed block store under `<data>/modal/volumes/`; block URLs served by sandboxd; HMAC-signed | M | Needs public HTTPS URL; 415 handler must route HTTP |
| 4c | Volume file API (v1) | `volume.py:1316-1465`, `_utils/blob_utils.py:282-346` | `VolumePutFiles`, `MountPutFile`, `BlobCreate`/`BlobGet` (S3 PUT with Content-MD5/ETag, multipart XML), `VolumeGetFile`, ... | Avoid: answer V2 | refuse explicit `version=1` | Emulating S3 ETag semantics |
| 4d | `Sandbox.create(volumes={...})`, `sb.reload_volumes()`, commit | `sandbox.py:1040-1042,1124`, `volume.py:473-483`, `sandbox.py:2005-2025` | `Sandbox.volume_mounts{volume_id, mount_path, read_only, sub_path, allow_background_commits}`; router `TaskReloadVolumes`; `VolumeCommit/VolumeReload` | Copy-in at boot / copy-out on terminate and reload via agent exec+tar; or NFSv3 from host (later) | M-L | Semantics of background commits UNVERIFIED |
| 4e | `CloudBucketMount`, `NetworkFileSystem` | `cloud_bucket_mount.py`, `network_file_system.py:37` (deprecated) | `cloud_bucket_mounts`, `nfs_mounts` | none (no FUSE in guest kernel) | out of scope (refused today) | — |
| 4f | `Dict`, `Queue` | — | — | not needed by sandboxes | out of scope | — |
| 5a | `sb.snapshot_filesystem()` | `sandbox.py:1678-1735` (router path; legacy `SandboxSnapshotFs` only with `MODAL_USE_LEGACY_FILESYSTEM_SNAPSHOT=1` on V1) | router `TaskSnapshotFilesystem{task_id, snapshot_id(uuid), ttl_seconds}` returns `image_id` | `Engine.CreateCheckpoint` (`engine/lifecycle_checkpoints.go:73`), then export the file to a Modal image store; boot via `CreateSpec.ImageDisk` | M | Engine has no exported "checkpoint file path/export" |
| 5b | `Image.from_id` (reuse a snapshot) | `_image.py:1016-1031` | `ImageFromId` | image registry lookup | S | — |
| 5c | `sb.snapshot_directory()`, `sb.mount_image()`, `sb.unmount_image()` | `sandbox.py:1737-1864` | router `TaskSnapshotDirectory`, `TaskMountDirectory`, `TaskUnmountDirectory` | tar out via exec; mount via checkpoint slot (`engine/lifecycle_mounts.go:49`, `internal/agent/checkpoint_mounts.go:142`) + overlayfs, or copy-in | M-L | Only 4 slots, read-only; engine API is checkpoint-of-self only |
| 5d | Exit snapshot | `sandbox.py:1593-1676` | `SandboxGetExitSnapshot[V2]` long poll, `oneof outcome{success{image_id}, pending, error}`; opt-in via `experimental_options_v2["enable_exit_snapshot"]` | checkpoint before `Engine.Delete` on exit/terminate | S (after 5a) | — |
| 5e | Memory snapshots | `sandbox.py:2419-2550`, `snapshot.py:61` | V2: router `TaskSnapshotMemory`, `SandboxRestoreV2`; V1: `SandboxSnapshot`, `SandboxSnapshotWait`, `SandboxRestore`; `SandboxSnapshotGet`; create needs `Sandbox.enable_snapshot` | warm snapshots exist only for suspend/resume of the same record and are discarded on IP change (`store.Record.BootIP`) | L | Fork under a new IP/vsock/tap |
| 6 | Public TLS front | `_utils/grpc_utils.py:358-404`, `_utils/task_command_router_client.py:272-288`, `client.py:155-158` | — | `--modal-public-url`, Traefik, health checks | S-M | Long streams, message sizes, CA stores |

## 1. Ports and tunnels

### Client
- `Sandbox.create(encrypted_ports=[...], h2_ports=[...], unencrypted_ports=[...])` builds
  `PortSpec{port, unencrypted, tunnel_type}`. `h2_ports` sets `tunnel_type=TUNNEL_TYPE_H2` and
  `unencrypted=false`. See `sandbox.py:1070-1074` (V2) and `:540-546` (V1).
  - `block_network` together with any port raises `InvalidError` client-side (`:1027`, `:896`).
- **Create-time cache** (`sandbox.py:1188-1195`): the client caches `create_resp.tunnels` only if
  `len(tunnels) == len(open_ports)`. So the server should return every tunnel in `SandboxCreateV2Response.tunnels`.
  Otherwise `tunnels()` calls `SandboxGetTunnelsV2{sandbox_id, timeout=50}`.
- **`SandboxGetTunnels` responses**: `GenericResult result` plus `repeated TunnelData{host, port,
  unencrypted_host?, unencrypted_port?, container_port}`. A result status of `GENERIC_STATUS_TIMEOUT` makes the
  client raise `SandboxTimeoutError` (`sandbox.py:1956-1958`). Otherwise the result is ignored, so it can be
  left empty.
- **`Tunnel`** (`_tunnel.py:18-58`):
  - `url` is `https://{host}` with `:{port}` unless the port is 443.
  - `tls_socket` is `(host, port)`.
  - `tcp_socket` is `(unencrypted_host, unencrypted_port)` and raises if `unencrypted_host` is empty.
  - Hosted Modal's `encrypted_ports` are therefore **TLS-terminated TCP**, not necessarily HTTP. `tls_socket`
    is meant for any protocol over TLS. The client comment "Unencrypted tunnel endpoints are not known at
    create time" (`:1188`) suggests raw TCP ports are allocated asynchronously.
- **`modal.forward(port)`** (`_tunnel.py:~150-205`): `TunnelStart{port, unencrypted, tunnel_type}` /
  `TunnelStop{port}`, refused unless `client.client_type == CLIENT_TYPE_CONTAINER` (`:186`). That is the client
  running *inside a Modal Function container* with container credentials. Out of scope for a sandbox API.
- **Connect tokens** (`sandbox.py:1967-2003`): `SandboxCreateConnectToken[V2]{sandbox_id, user_metadata (string,
  JSON-encoded if a dict), port (default 8080)}` returns `{url, token}`, which becomes `SandboxConnectCredentials`
  (`types.py:361`). Two documented semantics from the docstring: the token is for HTTP connections, and the
  proxy adds `user_metadata` "to the headers when forwarding requests".
  - UNVERIFIED (from memory of Modal docs, not in the client):
    - the token goes in `Authorization: Bearer <token>`, or a `_modal_connect_token` query parameter that
      sets a cookie;
    - the metadata header is `X-Verified-User-Data`.
- **`inbound_cidr_allowlist`** (`sandbox.py:700-702`, `:1152`): "CIDRs allowed to connect inbound to the
  sandbox (tunnels and connection tokens)".

### Wisp mapping
- **Dialing.** `engine.DialPort(ctx, m, port)` (`engine/portdial.go:27-65`) opens a raw TCP stream to
  `localhost:port` in the guest over vsock via wisp-agent `/internal/tcp`. It needs no guest NIC, so tunnels
  work even with `--net=false` or `block_network`.
  - Hosted Modal forbids ports with `block_network`, so do the same.
- **Pattern to copy.** `frontend/daytona/preview.go:17-140`: Host `<port>-<uuid>.<domain>`, an
  `httputil.ReverseProxy` whose `DialContext` is `portDial`, `FlushInterval: -1`, and stripping of the
  front end's own credentials. E2B does the same (`frontend/e2b/e2b.go:13-16`).
- **Do not key the host on sandbox ID + arbitrary port the way Daytona/E2B do.** Modal exposes only
  *declared* ports, unauthenticated. With `<port>-<sb-id>` routing, anyone who knows a sandbox ID could
  reach any port. Recommendation:
  - mint a random label per declared port at create, e.g. `t-<16 base32>.modal.sandbox.inevitable.fyi`;
  - keep `{label -> sandbox, port, h2, unencrypted}` in `Record.Ext["modal"]` meta and an in-memory index
    rebuilt on start;
  - return `TunnelData{host: label+"."+domain, port: 443, container_port}`.
  - Labels must be lowercase. Sandbox IDs already are (`[a-z0-9]`).
- **Serving modes.**
  - *HTTP mode (recommended first, S-M).* Traefik terminates TLS for `*.modal.sandbox.inevitable.fyi` and
    forwards HTTP/1.1 to sandboxd's modal listener. The modal `Handler` (`frontend/modal/modal.go:144-155`)
    must route by Host *before* the gRPC/415 check. It then reverse-proxies to `DialPort`.
    - Covers web apps, SSE and websockets (ReverseProxy handles Upgrade).
    - `h2_ports`: the proxy-to-guest leg needs an h2c transport (`http2.Transport{AllowHTTP: true, DialTLSContext: portDial}`).
      What hosted Modal speaks to the container on an H2 tunnel is UNVERIFIED; h2c prior-knowledge is the
      likely answer. The public side gets h2 via Traefik's ALPN.
    - Does not cover non-HTTP protocols over `tls_socket`.
  - *Passthrough mode (faithful, M).* A Traefik TCP router `HostSNIRegexp` for the wildcard with
    `tls.passthrough=true` sends raw TLS to a sandboxd tunnel listener. sandboxd terminates TLS itself:
    - the cert comes from the daemon's existing `publicCerts`/ACME code (`internal/daemon/daemon.go`, used
      for `--public-listen`), and DNS-01 is needed for a wildcard;
    - it reads SNI, picks ALPN (`h2` for h2 ports), and splices bytes to `DialPort`;
    - this is exactly Modal's semantics: any TCP protocol over TLS.
    - A TCP router with HostSNI takes precedence over HTTP routers on the same entrypoint, so the API host
      can stay an HTTP router.
- **`unencrypted_ports` (M, or refuse).** Traefik would need a static TCP entrypoint per port. Better:
  - sandboxd gets `--modal-tcp-ports 40000-40999` and `--modal-tcp-host tcp.modal.sandbox.inevitable.fyi`
    (an A record to the host's public IP, not proxied);
  - it listens on an allocated port per declared port and splices to `DialPort`;
  - it returns `unencrypted_host/port`, plus a TLS `host` for the same port as hosted does.
  - Requires the firewall and NAT to forward the range. Whether inevitable.fyi's ingress (Cloudflare? a home
    router?) allows that is UNVERIFIED.
  - Until then, refuse with `unsupported("unencrypted_ports")`.
- **Connect tokens (S-M).**
  - `url` is `https://c-<label>.<domain>` (or the sandbox's tunnel host).
  - `token` is HMAC- or JWT-signed `{sandbox, port, user_metadata, exp}`. Reuse the router JWT key
    (`modal.go:122-123`), but note that it rotates per process. Use a persisted key if tokens must survive
    restarts.
  - The proxy checks the token, strips it, and adds the metadata header.
- **`inbound_cidr_allowlist` (S).** Check `RemoteAddr` or the right-most untrusted XFF hop in the tunnel
  proxy. Only trust XFF when the front end is configured to be behind a proxy.
- **Lifecycle.** Modal sandboxes are never idle-suspended (`control.go:291-293`, `IdleNone`), so a tunnel
  never needs a wake. Still, use `f.acquire` as Daytona's `enter` does.

## 2. Network controls

### Client
- **Create-time** (`sandbox.py:1076-1095`; V1 `:278-300`): `Sandbox.network_access = NetworkAccess{
  network_access_type: OPEN|BLOCKED|ALLOWLIST, allowed_cidrs[], allowed_domains[]}`.
  - Neither allowlist means OPEN.
  - Either allowlist, including `[]`, means ALLOWLIST. An empty ALLOWLIST blocks all public egress; the
    client says so for i6pn at `:1030-1034`.
  - Domain syntax: exact, `*.` prefix, or bare `*` (`:697-699`).
  - `Sandbox.block_network` (field 11) is a legacy bool. 1.6.0 sends `network_access` instead.
- **Runtime**: `_experimental_set_outbound_network_policy(outbound_cidr_allowlist=, outbound_domain_allowlist=)`
  calls router `TaskSetNetworkAccess{task_id, network_access}` (`sandbox.py:1533-1563`). Both `None` means OPEN.
  The docstring says established connections the new policy no longer permits are terminated.
- **Outbound policy** (`modal/_outbound_policy.py`):
  - shape: `OutboundPolicy{header_replacements[{domain, secret_id, headers map}]}`, at most 25 headers;
  - `$KEY` templating from a *named* secret, and "secret values never enter the Sandbox";
  - incompatible with `block_network` and with `outbound_domain_allowlist` (`_outbound_policy.py:~150-165`);
  - runtime update via router `TaskSetOutboundPolicy`.

### Wisp mapping
- **netpolicy** (`internal/netpolicy/rules.go`):
  - a domain allowlist of `store.NetworkRule{Domain, Action}` plus `include: defaults`;
  - "more specific wins"; `*.example.com` covers **subdomains only, not the apex** (`rules.go:88-90`);
  - enforced by a policy DNS server, a transparent TCP proxy that only connects to addresses resolved from
    allowed names (`enforcer.go:178-198`), and nftables diversion through `wisp-netd`
    (`engine/egress.go:30-40`).
- **Applying it at create (S).**
  - Set `sp.Record.NetworkRules` before `f.life.Create` (`control.go:291`). `egress.admit` compiles it at boot
    (`egress.go:212-233`), and a restrictive policy that can't be enforced fails closed. `tapFor` boots
    without a NIC (`egress.go:269-288`), which for BLOCKED is the right outcome anyway.
  - `BLOCKED` means `[{Domain:"*",Action:"deny"}]`. Alternatively add a per-record "no NIC" flag. The engine
    only has a global `Options.NoNetwork` (`engine/lifecycle.go:67`).
  - `ALLOWLIST` with domains means one allow rule per domain. Modal's `*.x` probably includes the apex: the
    outbound_policy doc says so explicitly (`_outbound_policy.py`, "matching the apex domain and
    subdomains"), and the allowlist doc doesn't say. UNVERIFIED. Translating `*.x` into both `x` and `*.x`
    is safer for compatibility.
  - An empty ALLOWLIST is deny-all.
- **CIDRs (M).** netpolicy has no address rules. Add `[]netip.Prefix` to `Policy`. `Enforcer.authorize` then
  allows `dst` in a prefix without a DNS record.
  - Policy semantics: with CIDRs and no domains, DNS has to keep resolving (or clients can't find the hosts
    whose IPs are in range), but connections are allowed only to the prefixes. That is a new mode,
    "DNS open, connect gated".
  - `netpolicy.Blocked` still refuses private and host ranges whatever the CIDR (`proxy.go:24-27`). That is
    probably fine (hosted Modal surely blocks its own internals too), but document it.
  - UDP: restricted sprites get only DNS (the kernel drops the rest), so a CIDR rule won't allow UDP to that
    CIDR. Document it.
- **Runtime change (S).** `TaskSetNetworkAccess` maps to `Engine.SetNetworkPolicy(id, rules, compiled)`
  (`egress.go:310`), then `RepublishNetworkPolicy`. `Enforcer.track` re-checks on policy change and closes
  flows (`enforcer.go:202`), which matches Modal's documented behavior.
  - Going from OPEN to restricted requires the helper (`ErrUnenforceable`), which should map to
    `FAILED_PRECONDITION`.
- **`outbound_policy` (L, refuse).** wisp's proxy is L4 and never sees HTTP. Injecting headers into HTTPS
  needs a MITM proxy, a per-sandbox CA in the guest trust store, and HTTP/1.1+h2 rewriting. Refuse
  `Sandbox.outbound_policy` and `TaskSetOutboundPolicy` with `unsupported`.
- **Host prerequisite.** With `--net=false` (how the spike gate ran) OPEN can't be honoured either: sandboxes
  have no network at all. A public deployment needs the bridge and `wisp-netd` (`scripts/setup-host.sh`).

## 3. Secrets

### Client
- **Ephemeral kinds**: `Secret.from_dict`, `from_dotenv`, `from_local_environ` (`secret.py:298-430`) carry
  `_load_env_dict` (`_is_ephemeral`, `:289`).
  - **V2 create**: `_resolvable_secrets` (`secret.py:589`) drops them from `secret_ids`. `_local_secret_env`
    (`:594-604`) merges their values, with `env=` winning, into `SandboxCreateV2Request.ephemeral_secrets`
    (`sandbox.py:1120,1165-1170`). **No secret RPC is made.**
  - **exec**: the same inlining into `TaskExecStartRequest.env` (`sandbox.py:2316-2324`). Only named secrets
    go in `TaskExecStartRequest.secret_ids`.
  - **V1 create** (`_create`, `sandbox.py:899-901`): `env` becomes `Secret.from_dict(env)`, which loads through
    `SecretGetOrCreate{object_creation_type: ANONYMOUS_OWNED_BY_APP, env_dict, app_id}` (or EPHEMERAL with no
    app, `secret.py:216-233`). Its `secret_id` then goes in `Sandbox.secret_ids`.
- **Named**: `Secret.from_name(name, environment_name=, required_keys=)` calls
  `SecretGetOrCreate{deployment_name, environment_name, required_keys}`. The response is `{secret_id,
  metadata{name, keys[], environment_name, creation_info}}`. `metadata.keys` matters: `_local_secret_env`
  uses it so inlined values override named ones (`secret.py:600-603`). A missing name should be `NOT_FOUND`.
- **Management**:
  - `Secret.objects.create(name, env_dict, allow_existing=)`: `SecretGetOrCreate` with `CREATE_IF_MISSING` or
    `CREATE_FAIL_IF_EXISTS` (`secret.py:31-93`);
  - `.list` → `SecretList{environment_name, pagination}` → `items[{label, created_at, secret_id, metadata}]`;
  - `.delete` → `SecretDelete{secret_id}` after a from_name load;
  - `secret.update(env_dict)` → `SecretUpdate{secret_id, updates[{key,value}]}`, which merges;
  - `secret.info()` → `SecretGetInfo`.
  - The `modal secret` CLI uses the same RPCs.
- **Outbound-policy secrets** must be named (`_outbound_policy.py` `_validate`).

### Wisp mapping
- **V2 ephemeral (works today).** `SandboxCreateV2` reads `EphemeralSecrets` (`control.go:197-201`). Exec
  `env` passes through `router.go:224`.
- **V1 ephemeral (likely broken today).** `SandboxCreate` passes `env=nil` (`control.go:213`) and refuses
  `secret_ids` (`control.go:241`). Worse, the client calls `SecretGetOrCreate` first, which is
  UNIMPLEMENTED. Fix: implement `SecretGetOrCreate` for ANONYMOUS/EPHEMERAL (keep the values in memory, keyed
  by `st-...` id, bound to the app), and resolve `secret_ids` to env at create.
- **Server-side store (S-M).**
  - `<data>/modal/secrets.json`, mode 0600, keyed by `(environment, name)`, holding `id, keys, values,
    created_at`.
  - Optionally encrypt the values with a key file, as `--backup-key-file` does (`internal/daemon/flags.go:153`).
  - App-owned anonymous secrets die with the app (apps are never stopped today, so give them a TTL).
- **Where values go.**
  - Sandbox secrets become the sandbox env (like `m.Env`), applied to the entrypoint and every exec. Hosted
    execs inherit the container env; UNVERIFIED but very likely.
  - Exec `secret_ids` are resolved per exec.
  - **Two leaks in the current spike.** Both get worse once named secrets exist:
    1. `m.Env` is persisted in `Record.Ext["modal"]`, i.e. in the store's sprite JSON, in plaintext.
    2. Values are passed in **argv** (`frontend/modal/exec.go:38-52`, `sudo ... env K=V ...`). Anything in
       the guest can read them from `/proc/*/cmdline`, and they may show up in agent or host logs.

    Prefer the agent exec API's own env field (if `POST /exec` accepts one; check `internal/agent/session.go`)
    plus `sudo --preserve-env=<keys>`, or a 0600 env file in the guest that `env -S`/`set -a` reads. Store
    secret *ids*, not values, in the meta.
- **Auth.** Secret RPCs are control-plane calls. Only admin keys may create, update or delete. Add
  `SecretGetInfo` and `SecretList` to `readOnly` (`modal.go:208-212`) if read keys should see names (never
  values: no RPC returns values).

## 4. Volumes

### Client protocol
- **Lookup**: `VolumeGetOrCreate{deployment_name, environment_name, object_creation_type, app_id, version,
  create_options}` returns `{volume_id, version, metadata{version, name, creation_info}}` (`volume.py:277-345,
  708-770`).
  - `Volume.ephemeral()` uses `EPHEMERAL` plus a `VolumeHeartbeat{volume_id}` loop (`:821-869`). The server
    should delete the volume when heartbeats stop.
  - Also `VolumeGetById`, `VolumeList`, `VolumeDelete`, `VolumeRename`.
- **Version is server-chosen.** The client uses the v1 RPCs when `metadata.version` is UNSPECIFIED or V1, and v2
  otherwise (`volume.py:687-694`). An explicit `version=` that differs from the stored one is an
  `InvalidError` (`:238-257`). **Answering V2 for new volumes (request UNSPECIFIED or 2) keeps every client off
  v1.**
- **v2 upload** (`batch_upload`, `_VolumeUploadContextManager2`, `volume.py:1472-1632`):
  1. Files are split into **8 MiB blocks** (`_utils/blob_utils.py:63` `BLOCK_SIZE`), each sha256-hashed.
  2. `VolumePutFiles2{volume_id, files[{path, mode, size, blocks[{contents_sha256, put_response}]}],
     disallow_overwrite_existing_files}`.
  3. The server answers `missing_blocks[{file_index, block_index, put_url}]`.
  4. The client **HTTP `PUT`s each block body to `put_url`** (aiohttp, `_put_missing_blocks`, `:1634-1700`).
     It keeps the **response body bytes** and echoes them back as `Block.put_response` on the second
     `VolumePutFiles2`. The loop runs at most 2 times; `AlreadyExistsError` becomes `FileExistsError`.
  5. Errors: status ≥ 400 gives `ExecutionError`, and 503 gives `ServiceError` with retries
     (`volume.py:104-127`).
- **v2 download**: `VolumeGetFile2{volume_id, path, start, len, client_pads_blocks=true}` returns `{get_urls[],
  size, start, len}`.
  - There is one URL per 8 MiB-aligned block of the range (`_expected_block_lengths`, `volume.py:84-101`). The
    client `GET`s them.
  - A body may be **shorter** than the block (trailing zeros omitted, client pads) but never longer.
  - An optional `Repr-Digest: sha-256=:<b64>:` header is verified (`volume.py:138-222`).
  - `VolumeGetFile2` errors map to `FileNotFoundError` for a missing path.
- **Other v2 RPCs**:
  - `VolumeListFiles2{path, recursive, max_entries}` is a server stream of `{entries[FileEntry{path, type,
    mtime, size}]}`.
  - `VolumeRemoveFile2{path, recursive}`.
  - `VolumeCopyFiles2{src_paths, dst_path, recursive}`.
- **v1** is different and worse: `VolumePutFiles` with `MountPutFile` (data inline under 4 MiB, otherwise
  `BlobCreate` returns S3 `upload_url(s)`).
  - The single-part PUT carries `Content-MD5`, and the client **verifies the `ETag` as the md5**
    (`blob_utils.py:111-163`).
  - Multipart needs S3's completion XML and `"<md5-of-md5s>-N"` ETag (`:166-243`).
  - Downloads use `BlobGet` and a `download_url`.
  - Avoid v1.
- **Commit/reload**: `vol.commit()` sends `VolumeCommit{volume_id}` and returns `{skip_reload}`;
  `vol.reload()` sends `VolumeReload` (`volume.py:882-931`). These matter inside a container. From a laptop
  they are near-no-ops.
- **Sandboxes**:
  - `volumes={path: vol}` becomes `Sandbox.volume_mounts[VolumeMount{volume_id, mount_path,
    allow_background_commits=true, read_only, sub_path?}]` (`volume.py:473-483`, `sandbox.py:1124`).
  - `vol.with_mount_options(read_only=, sub_path=)`.
  - `sb.reload_volumes(timeout=55)` calls router `TaskReloadVolumes{task_id, container_id}` with a client
    deadline (`sandbox.py:2005-2025`, `task_command_router_client.py:1136-1149`).
  - The client never calls an explicit commit for a sandbox. Hosted v2 volumes in sandboxes commit in the
    background and at sandbox exit; a `sync <mount>` inside the sandbox is said to force a commit. Both
    UNVERIFIED.
  - The open-files check names `/__modal/volumes/<id>` as the in-container path (`volume.py:925`):
    hosted mounts live there, presumably bind-mounted to `mount_path`.
- **Uploads use certifi, the router does not.** The aiohttp data-plane session uses certifi's CA bundle
  (`_utils/http_utils.py:25-32`). URLs handed out must be https with a publicly-trusted cert, or http only
  when testing locally.

### Wisp mapping
- **Storage (M).**
  - Content-addressed blocks: `<data>/modal/volumes/blocks/<sha256>`, zero-padding stripped.
  - One manifest per volume: `path → {mode, size, mtime, blocks[]}`. JSON or bolt.
  - `VolumePutFiles2` answers `missing_blocks` with signed URLs, e.g.
    `https://<public>/_modal/blocks/<sha256>?exp=..&sig=..` (HMAC over hash and expiry).
  - The PUT handler verifies the sha256 of the body before storing.
  - `put_response` can be empty, or a server-signed receipt checked on the second call.
  - GET URLs serve block bytes with `Repr-Digest`.
- **`internal/backup` is a strong model.** It already does chunking (`chunk.go`), manifests, encryption and
  S3 (`internal/s3`), so volumes could later be durable in S3. `internal/s3` signs requests itself and has
  **no presign**, but sandboxd proxying the block URLs avoids needing presign.
- **Handler routing.** `modal.go:149-153` 415s every non-gRPC request. Route `/_modal/blocks/*` (and tunnels,
  `/healthz`) before that check. Both HTTP/1.1 and h2c reach the same `http.Server`
  (`internal/daemon/daemon.go:199-204`).
- **Getting a volume into a sandbox: the real design question.** The guest kernel is
  `vmlinux-6.1.155` from Firecracker CI (`scripts/fetch-deps.sh:8`). Grepping its symbol strings locally found
  `nfs_fill_super`, `xs_tcp_setup_socket` (sunrpc), `ovl_fill_super`/`ovl_mount` and `ext4_fill_super`, and
  **no** `fuse_dev_init`, `v9fs_mount` or virtio-fs. That is weak evidence (strings only), but: no FUSE, 9p or
  virtio-fs. Firecracker has no virtio-fs anyway. Options:
  - **A. Copy-in/copy-out via the agent (recommended, M-L).**
    - At boot, after acquire, stream a tar of the materialized volume (or `sub_path`) into `mount_path` with
      an agent exec that has stdin (`internal/agent/session.go:47,350`) and runs `tar -x`.
    - Commit on terminate or exit (before `Engine.Delete`), and on `TaskReloadVolumes` (reload commits
      first, as Modal's docs allow): `tar -c` out, diff against the boot manifest, apply last-writer-wins per
      file.
    - `read_only`: skip commit, and chmod or bind-ro in the guest.
    - Works without a NIC. Cost is O(volume size) at boot, so only suitable for small and medium volumes.
    - Concurrency semantics match Modal's documented ones: explicit commit/reload, last writer wins.
  - **B. NFSv3 from the host (L, later).** A userspace NFS server in sandboxd (e.g. go-nfs) on the bridge
    gateway, mounted in the guest with `mount -t nfs -o vers=3,nolock,port=..,mountport=..`. It is live and
    shared, which is stronger than Modal's semantics. Costs:
    - needs a NIC;
    - needs an exemption in netpolicy's `Blocked()` and the forward chain for the gateway port;
    - does not work with `block_network`;
    - wispd is unprivileged, which is fine for userspace NFS.
  - **C. Per-volume ext4 image on a hot-swapped drive** (the checkpoint slot mechanism). Single writer only,
    and the host needs root to write files into the image. Not recommended for RW volumes.
- **`TaskReloadVolumes` (S once A exists).** Re-sync every volume mount of the task. `container_id` non-empty
  means unsupported (sidecars).
- **`VolumeCommit`/`VolumeReload` from outside** answer `skip_reload: true` as no-ops. Calls from inside a
  sandbox don't happen: the guest has no Modal container credentials.
- **Out of scope**:
  - `CloudBucketMount`: hosted uses a FUSE S3 mount; there is no FUSE in the guest.
  - `NetworkFileSystem`: `network_file_system.py:37` deprecates it, and it forces V1 (`sandbox.py:851-853`).
  - Both are already refused (`control.go:243`, `:202`).
  - Dict and Queue: sandboxes don't need them.

## 5. Snapshots

### Client
- **`snapshot_filesystem(timeout=55, ttl=30d)`** (`sandbox.py:1678-1735`), for both V1 and V2: router
  `TaskSnapshotFilesystem{task_id, snapshot_id=uuid4, ttl_seconds (-1 = no expiry), container_id}`, which
  returns `{image_id}`.
  - The result is used with `_Image._new_hydrated(image_id)`: no `ImageGetOrCreate` follows, and
    `Sandbox.create(image=snap)` sends that `image_id` straight in `Sandbox.image_id`.
  - Legacy `SandboxSnapshotFs{sandbox_id, timeout}` returns `{image_id, result, image_metadata}`. It is used
    only with `MODAL_USE_LEGACY_FILESYSTEM_SNAPSHOT=1` on V1.
- **`Image.from_id(id)`** → `ImageFromId` (`_image.py:1016-1031`): how a snapshot is reused in another process.
- **`snapshot_directory(path, timeout=55, ttl=30d)`** → router `TaskSnapshotDirectory{task_id, path (bytes),
  snapshot_id, ttl_seconds, customer_supplied_encryption_key?}` → `{image_id}` (`sandbox.py:1811-1864`).
- **`mount_image(path, image)`** → `TaskMountDirectory{task_id, path, image_id, key?}` (`:1737-1787`).
  - Accepts built images, `Image.from_id`, fs/dir snapshots and `Image.from_scratch`, but not images with
    local mount layers (`:204-235`).
  - The path is created; existing content is "replaced by the mount".
  - `unmount_image(path)` → `TaskUnmountDirectory`; afterwards the underlying directory reappears.
- **Exit snapshot** (`:1593-1676`).
  - Opt-in: `experimental_options={"enable_exit_snapshot": True}`, sent as
    `experimental_options_v2{"enable_exit_snapshot":"True"}` (`:1156-1158`).
  - `SandboxGetExitSnapshot[V2]{sandbox_id, timeout}` is a long poll returning `success{image_id}`, `pending`,
    or `error{ERROR_CODE_TIMEOUT|INTERNAL|FILESYSTEM_INCONSISTENT, message}`.
- **Memory snapshots** (`:2419-2550`, `snapshot.py`). The sandbox needs `_experimental_enable_snapshot=True`,
  i.e. `Sandbox.enable_snapshot`.
  - V2: router `TaskSnapshotMemory{task_id, idempotency_key}` → `{snapshot_id}` (client timeout 165 s).
  - V1: `SandboxSnapshot` → `{snapshot_id}`, then `SandboxSnapshotWait{snapshot_id, timeout=55}`.
  - Restore: `Sandbox._experimental_from_snapshot(snap, name=...)`.
    - V2: `SandboxRestoreV2{snapshot_id, sandbox_name_override[_type]}` → `{sandbox_id, task_id,
      command_router_access, tunnels}`.
    - V1: `SandboxRestore`, then `SandboxGetTaskId{wait_until_ready}`.
  - `SandboxSnapshot.from_id` → `SandboxSnapshotGet` (returns `is_v2` in `SandboxSnapshotHandleMetadata`).

### Wisp mapping
- **Images (S).** `frontend/modal/state.go` `Images` maps an id to a disk tag, and `create()` only accepts
  `diskModal` (`control.go:252-256`). Generalize it to `image_id → {disk path, kind: debian_slim|fs-snapshot|
  dir-snapshot, expires}`, plus `ImageFromId`, and a GC loop for `ttl_seconds`.
- **`snapshot_filesystem` (M).**
  - `Engine.CreateCheckpoint(sp, nil, comment, info)` (`engine/lifecycle_checkpoints.go:73`, implementation
    `:135-193`) does presuspend sync, VM pause, `cp --reflink=auto`, and resume. That is exactly a consistent
    fs snapshot, and processes keep running.
  - But checkpoints live with the sprite and **`Engine.Delete` removes them** (`lifecycle_create.go:108-119`),
    while Modal snapshot images outlive the sandbox.
  - So add a small engine export: e.g. `ExportCheckpoint(sp, id, dst)` that reflinks `checkpointPath` (private,
    `:59`) under `HoldCheckpoint` (`:113`) into `<data>/modal/images/<im-id>.ext4`, then deletes the
    checkpoint. Or a direct `SnapshotDisk(sp, dst)` that skips the checkpoint record.
  - Boot from it with `engine.CreateSpec{ImageDisk: path}` (`lifecycle_create.go:22-36`).
  - Disk-guard admission applies (`l.disk.admit`).
  - The image contains wisp guest internals (`/.sprite`). That is harmless because the same guest image
    family boots from it.
- **`snapshot_directory` (M).** An exec `tar -C <path> -cf - .` through the agent, stored as tar. Turn it into
  an ext4 image with `mkfs.ext4 -d <dir>` (unprivileged; e2fsprogs ≥ 1.43) when needed for mounting.
- **`mount_image`.** Two options:
  - **(a)** Reuse the **checkpoint slot** mechanism: 4 hot-swappable read-only virtio drives
    (`internal/vmm/machine.go:44`, `engine/lifecycle_mounts.go:49-87`, agent `mountSlot` in
    `internal/agent/checkpoint_mounts.go:142-171`), plus an **overlayfs** upper in the guest (overlayfs is in
    the kernel) so the path is writable, then bind it to `path`.
    - Fast and O(1), and handles whole-rootfs images (fs snapshots, debian_slim) too.
    - But: at most 4 slots, shared with checkpoint mounts, and the engine API only mounts *the sprite's own
      checkpoints*, requested *from inside the guest* (`from *GuestChan`, `ErrStaleGuest`). It needs a
      host-initiated `MountImage(sp, file)` variant. **M-L.**
  - **(b)** Copy-in: move the existing dir aside, untar, and swap back on unmount. Unlimited and simple, O(size).
    **M.**
  - `customer_supplied_encryption_key`: refuse it.
- **Exit snapshot (S after 5a).** If the sandbox's options ask for it, take the fs snapshot in the
  exit/terminate path before `f.life.Delete`, and record the outcome in `state.json` for
  `SandboxGetExitSnapshot` (a long poll, like `SandboxWait`).
- **Memory snapshots (L, defer).**
  - wisp has warm snapshots: Firecracker mem+vmstate written on suspend and resumed in place
    (`vmm.HasSnapshot`, `engine/lifecycle_rules.go:107` `Suspend`). There is no fork. A warm snapshot taken
    under a different IP "is useless and gets discarded" (`store/store.go`, `BootIP`).
  - Restoring into a **new** sandbox needs:
    - copying mem, vmstate and disk under a new record;
    - Firecracker snapshot-load overrides for tap, vsock UDS and drive paths;
    - the guest re-addressing its NIC after resume, perhaps via the agent's `/internal/resumed` hook;
    - the egress enforcer keyed by the new IP.
  - Return `UNIMPLEMENTED` for `enable_snapshot`, `TaskSnapshotMemory`, `SandboxSnapshot*` and
    `SandboxRestore*` for now.
  - Whether hosted terminates the source sandbox after `_experimental_snapshot` is UNVERIFIED.

## 6. Going public (https://modal.sandbox.inevitable.fyi behind Traefik)

### What changes on the client side
- `MODAL_SERVER_URL=https://modal.sandbox.inevitable.fyi` gives a grpclib channel on 443 with `ssl=True`
  (`_utils/grpc_utils.py:358-380`). grpclib's default context uses certifi when installed
  (`grpclib/client.py:757-770`; modal depends on certifi), and ALPN must negotiate `h2`.
- **Router URL must be https and trusted.** The client isn't on localhost (`client.py:155-158`), so the router
  URL must be `https://` (`task_command_router_client.py:272-288`).
  - The router channel uses `ssl.create_default_context()`, i.e. the **system** CA store, not certifi.
  - A Let's Encrypt cert is fine on Linux. On python.org macOS builds without "Install Certificates" the
    router could fail where the control plane works. UNVERIFIED; hosted Modal would hit the same issue.
- `x-modal-host: modal.sandbox.inevitable.fyi` is sent on every control call (`grpc_utils.py:388-397`). It is
  informational; the server can ignore it.
- The router JWT (bearer) and the token headers pass through Traefik untouched. JWT keys rotate per process
  (`modal.go:122-123`). The client refetches on UNAUTHENTICATED, so that is fine.
- Keepalive: the client sends HTTP/2 PINGs every 30 s with a 10 s timeout, even without calls
  (`grpc_utils.py:207-209,304-330`). Traefik answers them.
  - grpc-go's ping enforcement doesn't apply because sandboxd serves gRPC via `ServeHTTP` on net/http's h2
    server (`modal.go:146`).
  - Windows: the client advertises 64 MiB stream and connection windows.

### sandboxd changes
- **`--modal-public-url https://modal.sandbox.inevitable.fyi`**, following `publicURL`/`reportedDomain` in
  `cmd/sandboxd/main.go:120-160` and the new `--sprites-public-url`. It sets:
  - `RouterURL`, the same URL, since one listener serves both services;
  - the tunnel domain, `*.modal.sandbox.inevitable.fyi`;
  - the volume block URL base and the connect-token URL base;
  - and should refuse `--modal-router-url` together with it.
- **Health.**
  - The modal `Handler` 415s anything non-gRPC (`modal.go:149-152`); E2B, Vercel, Daytona and Sprites all
    serve `GET /healthz` (`frontend/e2b/control.go:35`, `frontend/vercel/api.go:29`,
    `frontend/daytona/daytona.go:183`).
  - Add the same `/healthz` before the gRPC check.
  - Also register `google.golang.org/grpc/health` (`grpc.health.v1.Health/Check`) on the grpc server and
    let the auth interceptor pass it, so Traefik's `healthCheck.mode: grpc` works over h2c.
- **Message size.** grpc-go's default `MaxRecvMsgSize` is 4 MiB.
  - v1 `MountPutFile` inlines up to 4 MiB of data (`blob_utils.py:43`).
  - A large `VolumePutFiles2` (thousands of files × blocks) can exceed it, and so can big
    `TaskExecStdinWrite` payloads.
  - Set `grpc.MaxRecvMsgSize(64<<20)` or similar in `modal.go:145`.
- **Host routing in `Handler`**, in this order:
  1. tunnel and connect hosts go to the reverse proxy;
  2. `/_modal/blocks/` goes to the volume data plane;
  3. `/healthz`;
  4. gRPC;
  5. otherwise 415.

### Traefik
- **API router.** `Host(\`modal.sandbox.inevitable.fyi\`)` → service `h2c://sandboxd:7852`.
  - h2c is required: a gRPC backend must be HTTP/2, and sandboxd's listener speaks h2c
    (`internal/daemon/daemon.go:199-204`).
  - TLS from a normal cert resolver.
- **Tunnels router.** `HostRegexp(\`^[a-z0-9-]+\.modal\.sandbox\.inevitable\.fyi$\`)` → a **separate service
  `http://sandboxd:7852`** (HTTP/1.1).
  - Websocket upgrades through an h2c backend would need extended CONNECT (RFC 8441), which Traefik does not
    do toward backends, as far as I know. UNVERIFIED.
  - Needs a wildcard DNS record and a DNS-01 wildcard cert (`*.modal.sandbox.inevitable.fyi` plus the apex;
    a wildcard covers one label only, so `<label>.modal...` works but not `<a>.<b>.modal...`).
  - In passthrough mode, use a TCP router `HostSNIRegexp(...)` with `tls.passthrough: true` instead.
- **Timeouts.**
  - Entrypoint `transport.respondingTimeouts.readTimeout`: Traefik v3 defaults to 60 s; set 0 or ≥ 10 min.
    `writeTimeout` stays 0, and `idleTimeout` (180 s) is fine with pings.
  - Long server streams: `TaskExecStdioRead`, `SandboxStdioReadV2`, `ImageJoinStreaming`,
    `VolumeListFiles2`. The stdio streams stay open as long as the process writes, so hours are possible.
    They are server streams whose request body ends at once, so readTimeout mostly hurts large unary
    requests and slow block PUTs. The client resumes stdio from an offset after a drop
    (`task_command_router_client.py:~960-1000`), so a cut stream is survivable, not fatal.
  - `serversTransport.forwardingTimeouts.responseHeaderTimeout` stays 0; long-poll RPCs like `SandboxWait`
    (10 s), `TaskExecWait` (60 s) and `SandboxGetExitSnapshot` hold headers until done.
  - gRPC streaming responses are flushed immediately by Traefik.
- **Trailers.** gRPC status rides in HTTP/2 trailers, which Traefik passes for h2c backends.
- **Body size.** No buffering middleware on these routers; block PUTs are 8 MiB each.
- **Security once public.** `MODAL_TOKEN_SECRET` is a daemon root or API key and is now reachable from the
  internet. Consider rate limiting failed auth (`internal/ratelimit` exists) and recommend issuing scoped API
  keys rather than the root token. Tunnels are public by design, like hosted Modal's; connect tokens and
  `inbound_cidr_allowlist` are the access controls.

## Decisions for the owner

1. **`block_network` and allowlists today.** Refuse them as unsupported now (one line, honest), or implement
   §2's S path (domains plus BLOCKED)? Either way, stop ignoring them silently.
2. **Tunnel serving mode.** HTTP-only via Traefik (fast; no TLS-over-raw-TCP), or SNI passthrough with
   sandboxd terminating TLS with its own ACME wildcard (faithful; more moving parts)?
3. **Tunnel hostnames.** Random per-port labels (recommended: only declared ports reachable, unguessable), or
   Daytona-style `<port>-<sandbox-id>`?
4. **`unencrypted_ports`.** Open a public TCP port range on the host, bypassing Traefik, or refuse? This
   depends on whether inevitable.fyi's ingress can forward arbitrary ports.
5. **Domain wildcard semantics.** Should Modal's `*.x` include the apex `x` (translate to two wisp rules), or
   keep wisp's subdomains-only meaning?
6. **CIDR policies.** Extend netpolicy (shared by every API) with prefixes and a "DNS open, connect gated"
   mode, or refuse `outbound_cidr_allowlist`?
7. **Secret store.** Plaintext 0600 JSON or encrypted with a key file? And move env and secret delivery off
   argv and out of `Record.Ext` (affects the existing `env=` path too)?
8. **Volumes.**
   - v2-only (answer V2 and refuse `version=1`)?
   - Copy-in/out (no NIC needed, O(size)) or NFS (live, needs a NIC and a netpolicy exemption)?
   - Local disk only, or S3-backed blocks using `internal/s3`?
9. **Snapshot image lifetime.** Snapshot images must outlive their sandbox, so they need a small engine export
   API (`ExportCheckpoint`/`SnapshotDisk`). Who owns GC (TTL), and does disk-guard admission count them?
10. **`mount_image`.** Slot plus overlay (4-slot limit, engine API change) or copy-in?
11. **Memory snapshots.** Defer, i.e. answer UNIMPLEMENTED for `enable_snapshot`? Recommended.
12. **Public auth.** Allow the root token over the public endpoint, or require API keys and add auth-failure
    rate limiting?

## Unverifiable without hosted Modal

- The exact tunnel host format and port hosted Modal returns, and whether unencrypted endpoints show up later
  (the client comment implies async allocation).
- What protocol hosted Modal speaks to the container on `h2_ports` (h2c prior-knowledge?), and whether
  `encrypted_ports` are raw TLS-terminated TCP or HTTP-aware.
- Connect-token usage: header vs. query parameter vs. cookie, the metadata header name
  (`X-Verified-User-Data`?), and the URL shape.
- Whether `*.x` in `outbound_domain_allowlist` matches the apex; whether DNS stays open under a CIDR-only
  allowlist; how UDP is treated under an allowlist.
- Whether sandbox-level secrets (`secret_ids`, `ephemeral_secrets`) are visible to `exec`'d processes (almost
  certainly yes), and the error for a missing `required_keys`.
- When v2 volumes commit in a sandbox (background interval, at exit, on `sync`), and what `TaskReloadVolumes`
  does with uncommitted writes and open files.
- The server's default volume version for `version=None` in 1.6.0.
- `mount_image` writability (overlay or read-only) and exact replace/restore semantics.
- Whether `_experimental_snapshot` (memory) stops the source sandbox, and how restored sandboxes treat
  network identity and tunnels.
- Exit-snapshot timing and the `FILESYSTEM_INCONSISTENT` conditions.
- Whether hosted Modal ever uses the legacy `SandboxSnapshotFs` path for 1.6.0 clients (only with the env var
  client-side).
- The guest kernel FS findings (NFS and overlay present; FUSE, 9p and virtio-fs absent) come from `grep -a` of
  `~/.local/share/wisp/kernel/vmlinux`, not from a kernel config. Confirm with the Firecracker CI 6.1 config
  or by booting and reading `/proc/filesystems`.
- Also not run here: V1 `env=` failing (it reasons from `sandbox.py:899-901` and the UNIMPLEMENTED
  `SecretGetOrCreate`); check with `MODAL_SANDBOX_V2=0`.
