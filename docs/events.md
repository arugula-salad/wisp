# Events and webhooks

The Sprites API pushes nothing: to follow sprites, a client polls. This is our own addition,
outside upstream's `/v1` so it cannot collide with anything the official API has or adds.
Everything that changes a sprite reports to one in-process event bus, which serves:

- a **server-sent event stream** at `GET /mini-sprites/v1/events` on the API (bearer token);
- **webhooks**, POSTed to URLs the operator names with `--webhook`;
- the same stream **from inside a spawner sprite**, limited to the sprites it created.

## An event

```json
{"id": 1790067843360002, "type": "sprite.woke", "time": "2026-09-22T09:04:11.566946657Z",
 "sprite": "dev", "sprite_id": "fcc31a0c36d7", "detail": {"mode": "cold", "ms": 238}}
```

`id` goes up by one per event. It is seeded from the clock at startup (milliseconds × 1000),
so IDs keep increasing across restarts and are unique enough for a webhook receiver to
de-duplicate on; they stay below 2^53, so JavaScript reads them exactly. `parent_id` is set
for a sprite created from inside another. `detail` is small and depends on the type:

| Type | When | `detail` |
|---|---|---|
| `sprite.created` | created (by the API or a spawner) | `from: {sprite, checkpoint}` for a clone |
| `sprite.deleted` | deleted | |
| `sprite.woke` | booted or resumed | `mode` (`cold`/`warm`), `ms` (time to a running agent), `warm_discarded` (why a warm sprite booted cold, if it did) |
| `sprite.wake_failed` | a wake failed | `error` |
| `sprite.suspended` | snapshotted to disk (warm) | `ms`, `idle` (idle timeout, or the operator), `snapshot_bytes` (disk the memory snapshot takes) |
| `sprite.cold` | memory state dropped | `reason`: `warm ttl`, `operator`, `disk space` (+ `for`) |
| `sprite.stopped` | VM killed without a snapshot | `reason` when it was for lack of room |
| `sprite.exited` | the VM exited without being asked (guest reboot or poweroff, a crash) | |
| `checkpoint.created` / `.deleted` / `.restored` | | `checkpoint`, `auto` / `pruned` |
| `service.started` | a service process started | `service`, `pid`, `restart_count` on a restart |
| `service.crashed` | it exited on its own; a restart is scheduled | `service`, `exit_code`, `restart_count`, `restart_in_ms` |
| `service.stopped` | it was stopped on purpose (stop, restart, delete, redefine) | `service`, `exit_code` |
| `service.failed` | it could not be launched | `service`, `error`, `restart_in_ms` |
| `policy.changed` | a policy was set or removed | `policy` (`network`, `privileges`, `resources`, `spawn`) |
| `policy.denied` | the network policy refused a lookup or connection; a sprite without a spawn policy asked to spawn | `policy`, and for network `kind` (`dns`/`connect`), `target`, `reason` |
| `limit.refused` | `--max-sprites`, `--max-running`, `--max-running-memory-mib`, `--max-concurrent-boots`, a spawner's `max_children` or `--guest-checkpoint-limit` said no | `limit` (which one: `max_sprites`, `max_running`, `max_running_memory`, `max_concurrent_boots`, `max_children`, `guest_checkpoints`), `max`, `current` |
| `disk.refused` | the disk guard refused a create, checkpoint or restore | `operation`, `needed_bytes`, `free_bytes`, `reserve_bytes` |
| `disk.low` / `disk.ok` | the volume crossed `--disk-warn-percent` (repeated every 10 min while low) | free and total bytes |

`service.*` events come from the agent inside the guest, the one place that sees a service
exit. They are therefore claims the guest makes about itself: checked for shape and rate
limited (a burst of 30, then 5 a second per sprite), but a guest can lie about its own
services. They need an agent from this version, which a sprite gets on its next cold boot.
`policy.denied` for the network is rate limited the same way; the log still has every one.

## The stream

```sh
curl -N -H "Authorization: Bearer $SPRITE_TOKEN" \
  "$SPRITES_API_URL/mini-sprites/v1/events?sprite=dev,web&type=sprite.,service.crashed"
```

- **Filters**: `sprite` (names) and `type` (prefixes), each comma-separated or repeated.
- **Framing**: each event is one unnamed message, `id: <id>` and `data: <json>`, so
  `EventSource.onmessage` gets all of them. After any replay the stream sends the current
  position as an id with no data, which moves a client's resume point without delivering
  anything.
- **Heartbeats**: a `: ping` comment every 15 s keeps proxies from closing an idle stream.
- **Resuming**: send `Last-Event-ID` (an `EventSource` does on reconnect) or
  `?last_event_id=` and the stream first replays the buffered events after it. `0` replays
  the whole buffer. The buffer is the newest 1024 events, in memory: if the ID has fallen out
  of it (or is from before a restart), the stream says so with a `stream.gap` notice
  (`detail: {requested, oldest}`) before replaying what it has.
- **Slow readers**: a stream more than 256 events behind is closed with a `stream.lagged`
  notice rather than slowing anything down; reconnecting with `Last-Event-ID` picks up where
  it stopped, as long as the buffer still holds it. On shutdown, streams end with
  `stream.shutdown`. Notices carry no `id`, so they never move a resume point.

Replays and filters apply together, and the order is always ID order.

## From inside a sprite

A sprite with a [spawn policy](api.md#sprites-that-create-sprites) can follow the sprites it
created, and nothing else: every event whose `parent_id` is itself. That is the lobby case, a
front sprite showing live status of the games it handed out, with no token inside it.

```sh
sprite-env sprites events                          # follow, one JSON event per line
sprite-env sprites events --all --type service.    # start with what is still buffered
sprite-env sprites events --sprite game-42 --count 1 --type sprite.deleted   # wait for one
# or: curl -N --unix-socket /.sprite/api.sock http://sprite/mini-sprites/v1/events
```

The stream does not keep the sprite awake. Suspending it cuts the connection (vsock does not
survive a snapshot); `sprite-env` reconnects on its own after the wake and resumes after the
last position it was given, so a follower loses nothing that is still buffered. A sprite
without a spawn policy gets `403 spawn_disabled`, as for the other spawner routes.

## Webhooks

```sh
spritesd --webhook https://hooks.example.com/sprites --webhook-types sprite.,service.crashed
```

- `--webhook` is repeatable; each URL gets every event (or those matching
  `--webhook-types`) as the event JSON in a `POST`.
- Each delivery is signed. `X-Mini-Sprites-Timestamp` is Unix seconds, and
  `X-Mini-Sprites-Signature` is `sha256=` and the hex HMAC-SHA256, keyed with the webhook
  secret, of `<timestamp>.<body>`. The secret is `--webhook-secret-file`, by default
  `<data>/webhook-secret`, generated on first use. Verify in constant time and reject old
  timestamps to stop replays. `X-Mini-Sprites-Event` and `X-Mini-Sprites-Event-Id` carry the
  type and ID.
- A network error, `5xx` or `429` is retried 5 times, 1 s, 2 s, 4 s, 8 s and 16 s apart; any
  other `4xx` is final. Deliveries to one URL are in order, one at a time.
- Each URL has a queue of 1024 events. When a receiver is slow or down long enough to fill
  it, new events are dropped and counted, never waited for. `GET /mini-sprites/v1/webhooks`
  shows per URL: queued, delivered, failed, dropped and the last error (credentials and query
  strings in URLs are redacted, there and in the log).

## What this is not

- Nothing is persisted: the buffer and the webhook queues are memory. Events published while
  the daemon is down do not exist, and a restart loses whatever webhooks had not delivered.
  Treat the stream as a way to hear about changes promptly, and the API as the state.
- There is no delivery guarantee beyond the above: webhooks are at most 6 tries.
- The web UI does not use the stream yet; its activity lane still samples states every 5 s.
- A long-lived stream shows in the Traffic page's request metrics as one long request when it ends.
