# Lifecycle: running, warm, cold

| State | What exists | Wake |
|---|---|---|
| `running` | Firecracker process | – |
| `warm` | memory snapshot on disk, processes frozen | ~15 ms VM restore |
| `cold` | disk only | ~200 ms boot to a responsive agent |

A sprite suspends after `--idle-timeout` (30s) with nothing keeping it awake.

- **Keeps it awake:** an API or sprite-URL request in flight, an attached exec session or a
  running control-channel operation, session I/O, an open filesystem watch, a live task, and
  checkpoint calls made from inside.
- **Does not:** services and their output, port notifications, filesystem events, and idle
  pooled `/control` or `ports/watch` sockets (these are closed at suspend).

It goes cold after `--warm-ttl` (1h), which drops memory state: processes are gone, the
filesystem is intact. TCP connections never survive a suspend. The guest clock is stepped to
host time on every resume. On SIGTERM, spritesd suspends every running sprite so they resume
warm after a restart.

**Tasks** are explicit keep-awake holds, created from inside the sprite as upstream does:

```sh
curl --unix-socket /.sprite/api.sock -X POST http://sprite/v1/tasks -d '{"name":"build","expire":"10m"}'
```

They last at most 1h, are refreshed with PUT, and do not survive a cold boot or a spritesd
restart. `/v1/sprites/{name}/tasks` exposes the same thing from outside (our extension).
