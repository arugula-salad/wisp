# Modal 1.6.0 against wisp: exec, stdio, PTY, stdin and the sandbox filesystem

Scope: what the unmodified `modal` 1.6.0 client does for `sb.exec`, `ContainerProcess`, stdio streams, stdin, PTY,
sandbox-level stdio, and `sb.filesystem`. For each one: what wisp's sandboxd (`frontend/modal`) and wisp-agent
(`internal/agent`) do today, and what is missing.

Path conventions:
- `M/` = `.../modal16/.venv/lib/python3.14/site-packages/modal/`
- `W/` = `/home/jake/dev/arugula-salad/wisp/`

Wisp line numbers are from `main` at b3d697f.

---

## 1. Summary

- **Every exec, stdio and filesystem operation goes through the TaskCommandRouter gRPC service.** That holds for V1
  and V2 sandboxes. The only exceptions are V1 *sandbox-level* stdio, which uses control-plane `SandboxGetLogs` /
  `SandboxStdinWrite`. 1.6.0 has no legacy file API: no `sb.open`, `FileIO`, `sb.ls`/`mkdir`/`rm`, or
  `ContainerFilesystemExec*`. `grep` finds none of them. `MountPutFile` only appears in `mount.py`/`volume.py`.
- **`sb.filesystem` is built entirely on `sb.exec`.** It runs `/__modal/.bin/modal-sandbox-fs-tools '<json>'`.
  Results come back as JSON or raw bytes on stdout. Errors come back as one JSON object on stderr plus a non-zero
  exit. Writes stream into the process's stdin with the client-streaming `TaskExecStdinWriteStream` RPC. Watch emits
  JSON lines and is stopped by closing stdin. Section 3 reconstructs the full contract. Since the RPC layer is
  ordinary exec, **wisp needs no new filesystem RPCs. It needs (a) exec stdin and (b) a binary at that path.**
- **Router RPCs today: 4 of the 9 this area needs.** `TaskExecStart`, `TaskExecStdioRead`, `TaskExecWait` and
  `TaskExecPoll` exist (`W/frontend/modal/router.go`). Missing:
  - `TaskExecStdinWrite`, `TaskExecStdinWriteStream`, `TaskExecStdinStatus` (exec stdin);
  - `SandboxStdioReadV2`, `SandboxStdinWriteV2` (V2 sandbox stdio);
  - V1 control-plane `SandboxGetLogs`, `SandboxStdinWrite`.

  None of these is in `prune.py`'s `KEEP` list (`W/frontend/modal/modalpb/prune.py:18-44`), so codegen comes first.
- **Exec runs without stdin today, which is a semantic bug as well as a gap.** `agentExec` posts `stdin=false`
  (`W/frontend/modal/exec.go:65`), so the process gets `/dev/null`. On hosted Modal, stdin is an open pipe until
  `write_eof()`: `sb.exec("cat").wait()` blocks there, while on wisp it returns 0 at once. The fix is to move the
  frontend from `POST /exec` to the agent's exec WebSocket (`GET /exec?stdin=true[&tty=true&cols=&rows=]`). That
  socket already does stdin, EOF, resize and signals (`W/internal/agent/server.go:240-283`), and
  `W/frontend/daytona/agent.go:117-205` already has a client for it.
- **PTY is mostly plumbing:** the agent already has PTY sessions (`session.go:318-334`). An exec PTY is fixed-size:
  there is no resize RPC, and the window size comes only from `PTYInfo.winsz_rows/cols` at start.
- **`Sandbox.create(pty=True)` does NOT force V1 in 1.6.0.** Only the deprecated `pty_info=` argument does
  (`M/sandbox.py:851-853`). `pty=True` is turned into `pty_info` inside `_experimental_create`
  (`M/sandbox.py:1065-1068,1142`) and sent to `SandboxCreateV2`. Today wisp rejects it at `control.go:236`.
  `docs/providers/modal.md:25,163` ("PTY forces V1") is wrong for `pty=True` and should be corrected.
- **Sandbox-level stdio (`sb.stdout`/`sb.stderr`/`sb.stdin`) needs entrypoint output to be kept and readable after
  the sandbox ends.** Today the entrypoint's output is discarded (`startExec(..., "")`). Both access RPCs refuse a
  finished sandbox (`runningSandbox`, `control.go:506,514`), and so does `forTask` (`router.go:184-186`). Hosted
  `SandboxStdioReadV2` "serves both live reads and post-exit reads"
  (`M/_utils/task_command_router_client.py:610`).
- **fs-tools recommendation:** a small static Go implementation of the contract. Ship it the same way wisp already
  ships `sprite-env` (initramfs → disk at every boot, `W/cmd/wisp-agent/init.go:90-117`), but at
  `/__modal/.bin/modal-sandbox-fs-tools`, preferably on a tmpfs mounted at `/__modal` for Modal VMs only. Use raw
  inotify for watch so renames pair up the way Rust `notify` pairs them. Effort: **M** for the binary, **M** for
  exec stdin, so the filesystem API works end to end in roughly L total including tests.

---

## 2. Method → RPC / binary command → status → effort

"Status today" is wisp at b3d697f. S = under a day, M = 1-3 days, L = about a week.

| Client surface | RPCs / fs-tools command | Status today | Effort |
|---|---|---|---|
| `sb.exec(*args, stdout, stderr, timeout, workdir, env, secrets(local), text, bufsize)` | `TaskExecStart` | Works (`router.go:203-234`). Secrets: local `Secret.from_dict` is merged into `env` client-side and works; named secrets send `secret_ids` and are rejected | done |
| `sb.exec(..., secrets=[Secret.from_name(...)])` | `TaskExecStart.secret_ids` | `unsupported` (`router.go:218`) | out of area |
| `sb.exec(..., pty=True)` / `pty_info=` / `_pty_info=` | `TaskExecStart.pty_info` | `unsupported("PTY execs")` (`router.go:216`) | M (with the WS switch) |
| `p.stdout` / `p.stderr` `read()`, iteration, `bufsize=1` lines, `text=False` bytes, `StreamType.STDOUT` / `DEVNULL` | `TaskExecStdioRead` (server stream, offset-resumable) | Works (`router.go:243-275`). Gaps: silent truncation past 64 MiB per stream (`router.go:46,59-64`); output kept until the sandbox ends | S (surface truncation) |
| `p.wait()` / `p.poll()` / `p.returncode` | `TaskExecWait` (60 s long-poll, retried forever) / `TaskExecPoll` | Works (`router.go:297-328`) | done |
| `p.stdin.write()` + `drain()` / `write_eof()` | `TaskExecStdinWrite(task_id, exec_id, offset, data, eof)` | Missing: no RPC, and the process stdin is `/dev/null` | M |
| fs write path (`_stdin_write_stream`) | `TaskExecStdinWriteStream` (client stream: Start{offset}, data*, End) + `TaskExecStdinStatus` (resume) | Missing | M (shares the stdin machinery) |
| `p.attach()` (interactive, `modal shell`) | Start(pty) + StdioRead + StdinWrite (no resize, no heartbeat RPC) | Missing (PTY + stdin) | S on top of the above |
| `sb.stdout` / `sb.stderr` (V2, default) | `SandboxStdioReadV2(task_id, offset, fd)` → `{data, starting_offset}` | Missing; entrypoint output discarded | M |
| `sb.stdin` (V2) | `SandboxStdinWriteV2(task_id, offset, data, eof)` | Missing; entrypoint has no stdin | S-M |
| `sb.stdout` / `sb.stderr` (V1, `MODAL_SANDBOX_V2=0`) | control plane `SandboxGetLogs` (stream of `TaskLogsBatch`, `last_entry_id` resume, `eof`) | Missing | M |
| `sb.stdin` (V1) | control plane `SandboxStdinWrite(sandbox_id, input, index, eof)` | Missing | S |
| `Sandbox.create(pty=True)` | `SandboxCreateV2.definition.pty_info` | `unsupported("PTY sandboxes")` (`control.go:236`) | S once PTY exec exists |
| `sb.filesystem.read_bytes` / `read_text` / `copy_to_local` | exec `{"ReadFile":{"path":P}}` | No binary (fails as exit 127 → `SandboxFilesystemError`) | part of fs-tools M |
| `sb.filesystem.write_bytes` / `write_text` / `copy_from_local` | exec `{"WriteFile":{"path":P}}` + stdin stream | No binary, no stdin | fs-tools + stdin |
| `sb.filesystem.list_files` | exec `{"ListFiles":{"path":P}}` | No binary | fs-tools |
| `sb.filesystem.stat` | exec `{"Stat":{"path":P}}` | No binary | fs-tools |
| `sb.filesystem.make_directory(create_parents=True)` | exec `{"MakeDirectory":{"path":P,"parents":B}}` | No binary | fs-tools |
| `sb.filesystem.remove(recursive=False)` | exec `{"Remove":{"path":P,"recursive":B}}` | No binary | fs-tools |
| `sb.filesystem.watch(filter, recursive, timeout)` | exec `{"Watch":{...}}`, JSON lines; stopped by stdin EOF | No binary, no stdin | fs-tools (inotify) + stdin |
| Sidecar `container.exec` / `container.filesystem` | same RPCs with `TaskExecStartRequest.container_id` | `unsupported` (`router.go:220`) | out of area |
| Legacy `sb.open` / `sb.ls` / `sb.mkdir` / `sb.rm` / `FileIO` | n/a | **Not in 1.6.0** | none |

---

## 3. The `modal-sandbox-fs-tools` contract (reconstructed)

Sources:
- `M/_utils/sandbox_fs_utils.py`: the `make_*_command` builders at lines 115-332 and the `raise_*_error` helpers at
  96-306;
- `M/sandbox_fs.py`: callers and parsing;
- `M/types.py:39-55,318-357`: `FileType`, `FileWatchEventType`, `FileInfo`, `FileWatchEvent`;
- `M/exception.py:350-395`: exception classes.

The builders' docstrings say the JSON "must match the `Command` enum in the modal-sandbox-fs-tools Rust crate
(crates/modal-sandbox-fs-tools/src/lib.rs)" and that the schema is append-only, "like protobuf".

### 3.1 Invocation

- Path: `/__modal/.bin/modal-sandbox-fs-tools` (`M/sandbox_fs.py:39`).
- argv: exactly `[path, <one JSON string>]`. The client calls `container.exec(PATH, make_X_command(...))`.
- The JSON is serde's externally tagged enum: `{"<Variant>": {fields}}`. It is produced by `json.dumps`, so it uses
  default separators and ASCII escapes for non-ASCII path characters. Parse it as JSON; do not compare strings.
- It runs as an ordinary exec: the sandbox's default user (root on Modal), the sandbox env and the default workdir.
  The client never passes `workdir`, `env` or `timeout` for it.
- The client always passes **absolute** paths. `validate_absolute_remote_path` raises `InvalidError` client-side for
  anything else (`sandbox_fs_utils.py:335-337`). A robust implementation still resolves relative paths against the
  cwd.
- Exec options per operation:

  | Operation | `text` | `bufsize` | Notes |
  |---|---|---|---|
  | `read_bytes` / `copy_to_local` | `False` | default | `sandbox_fs.py:159,308` |
  | `read_text` | `True` | default | stdout is decoded as strict UTF-8 client-side; invalid UTF-8 raises `UnicodeDecodeError` in the client, not a binary error |
  | `watch` | `True` | `1` | line-buffered (`sandbox_fs.py:523`) |
  | everything else | `True` | default | |

  stdout and stderr are always PIPE.

### 3.2 Success and failure channel (all commands)

- **Success:** exit code 0. stdout carries the per-command payload below. Nothing on stderr is required; stderr is
  read but ignored on success.
- **Failure:** any **non-zero** exit code. The value is used only in the fallback message, "Operation on '{path}'
  failed with exit code {rc}". On failure, **all of stderr must be exactly one JSON object** (surrounding whitespace
  is allowed). The client strips stderr and calls `json.loads` on the whole thing, so a log line before it defeats
  parsing (`sandbox_fs_utils.py:41-60`):
  ```json
  {"error_kind": "<Kind>", "message": "<non-empty human text>", "detail": "<optional string>"}
  ```
  - `error_kind` must be a string. `message` must be a non-empty, non-whitespace string. `detail` is optional; a
    non-string becomes `""`. Extra keys are ignored.
  - If parsing fails, the client raises a generic `SandboxFilesystemError("Operation on '<path>' failed with exit
    code N")` and logs the raw stderr at debug level.
  - **Message convention:** for a recognised kind, the client raises `Exc(f"{message}: {remote_path}")`. For an
    unrecognised kind it raises `SandboxFilesystemError(message)` without the path. So `message` should **not**
    contain the path; use something like `"No such file or directory"`.
- **Exec-level failures** (the exec RPCs themselves fail) are translated by `translate_exec_errors`
  (`sandbox_fs_utils.py:340-348`):
  - `NotFoundError`, `ServiceError` and `ConnectionError` become `NotFoundError("The Sandbox is unavailable...")`;
  - any other `modal.Error`, including `ConflictError`, becomes `SandboxFilesystemError("An unexpected error
    occurred, please contact support@modal.com[ (Error code: XXXXXXXX)]")`. The 8-character code is extracted from
    the exception text with `Error code:\s*([A-Z0-9]{8})`.

### 3.3 Exception table (`error_kind` → Python exception, per command)

`message` is the payload's message and `P` is the path the client passed. "Other" means any other kind, which
raises `SandboxFilesystemError(message)`.

| error_kind | ReadFile | WriteFile | ListFiles | Stat | MakeDirectory | Remove | Watch |
|---|---|---|---|---|---|---|---|
| `NotFound` | NotFound | **other** | NotFound | NotFound | NotFound | NotFound | NotFound |
| `PermissionDenied` | Permission | Permission | Permission | Permission | Permission | Permission | Permission |
| `IsDirectory` | IsADirectory | IsADirectory | other | other | other | other | other |
| `NotDirectory` | other | NotADirectory | NotADirectory | NotADirectory | NotADirectory | other | other |
| `AlreadyExists` | other | **NotADirectory** | other | other | other | other | other |
| `IsFile` | other | other | NotADirectory | other | other | other | other |
| `PathAlreadyExists` | other | other | other | other | PathAlreadyExists | other | other |
| `DirectoryNotEmpty` | other | other | other | other | other | DirectoryNotEmpty | other |
| `NotSupported` | other | other | other | other | `InvalidError("…not supported for CloudBucketMounts")` | same InvalidError | same InvalidError |
| `FileTooLarge` | FileTooLarge | other | other | other | other | other | other |

The exception names in the table drop the `SandboxFilesystem` prefix and `Error` suffix; for example, NotFound is
`SandboxFilesystemNotFoundError`. Line references: write 96-112, list 126-142, read 156-174, remove 188-208, mkdir
222-244, stat 258-274, watch 288-306.

`WriteFile` with `AlreadyExists` → NotADirectory is not a typo. It reproduces Rust's `create_dir_all`, which returns
`EEXIST`/`AlreadyExists` when a parent component exists but is a regular file. In WriteFile, a parent component that
is a file should therefore produce `NotDirectory` (or `AlreadyExists`); both map to NotADirectory.

### 3.4 Commands

**ReadFile** — `{"ReadFile":{"path":P}}`
- stdout: the file's raw bytes, streamed, with nothing else. The client concatenates stdout
  (`sandbox_fs.py:306-319`) or streams it to a temp file and renames (`copy_to_local`, 156-184).
- Errors:
  - `NotFound`;
  - `IsDirectory` when P is a directory;
  - `PermissionDenied`;
  - `FileTooLarge` above the read limit. The hosted limit is unknown (UNVERIFIED);
  - the final symlink is presumably followed, as with `open(2)`.
- Wisp gotcha: the frontend keeps at most 64 MiB per exec stream and silently drops the rest
  (`router.go:46,59-64`). A larger file would come back truncated with exit 0. fs-tools must enforce a
  `FileTooLarge` limit no higher than the frontend's per-stream cap (or the cap must be raised and made an error).

**WriteFile** — `{"WriteFile":{"path":P}}`
- stdin: the file contents, read until EOF. The client streams them with `TaskExecStdinWriteStream` in 256 KiB
  chunks and sends EOF with an explicit `End` message (`task_command_router_client.py:40,657-748`).
- Semantics, from the docstrings at `sandbox_fs.py:80-118,559-618`:
  - create missing parent directories;
  - overwrite an existing file (truncate);
  - an empty stdin writes an empty file.
- stdout: ignored. Exit 0 on success.
- Errors:
  - `IsDirectory` when P is a directory;
  - `NotDirectory` or `AlreadyExists` when a parent component is a file;
  - `PermissionDenied`.
- **Early exit:** if the binary fails before draining stdin (for example `IsDirectory`), "the worker reports the
  dropped stdin write as ConflictError" (`sandbox_fs.py:633-641`). The client then calls `poll()`:
  - if the process has exited, it reads stderr and the exit code and raises the mapped error;
  - if it is still running, it re-raises `ConflictError` and the client then reports an unexpected filesystem
    error (see 3.2).

  So the binary **must exit promptly on error, without reading the rest of stdin**, and the server must answer
  stdin writes to an exited process with `FAILED_PRECONDITION` (or `ABORTED`).
- UNVERIFIED:
  - whether the hosted write is atomic (temp file + rename) or in place;
  - the mode of a new file (presumably 0644 under umask 022);
  - whether an overwrite keeps the old mode and owner;
  - the owner of new files and directories (root).

**ListFiles** — `{"ListFiles":{"path":P}}`
- stdout: one JSON **array** of FileInfo objects (3.5), one per directory entry, excluding `.` and `..`
  (`sandbox_fs.py:212-239`).
- Errors:
  - `NotFound`;
  - `IsFile` or `NotDirectory` when P is not a directory. Wisp-agent's `/fs/list` returns a one-entry list for a
    file (`W/internal/agent/fs.go:230-260`), which differs, so do not reuse that behaviour;
  - `PermissionDenied`.
- Ordering: UNVERIFIED. Rust `read_dir` order is unspecified; sorting by name is harmless.
- A symlink to a directory as P is presumably followed (`read_dir`), while entries are described with `lstat`.

**Stat** — `{"Stat":{"path":P}}`
- stdout: one JSON **object**, a FileInfo describing P **without following a final symlink** (`lstat`, per the
  docstring at `sandbox_fs.py:410-416`).
- Errors:
  - `NotFound`;
  - `NotDirectory` when a non-leaf component is not a directory (`ENOTDIR`);
  - `PermissionDenied` when a component is not searchable.

**MakeDirectory** — `{"MakeDirectory":{"path":P,"parents":B}}` (the client argument is `create_parents`, default
True)
- `parents=true`: `mkdir -p`. It is idempotent: success if P already exists as a directory.
- `parents=false`: the immediate parent must exist and P must not.
- stdout: ignored.
- Errors:
  - `NotFound` when the parent is missing and `parents=false`;
  - `PathAlreadyExists` when P exists. For `parents=true` this means P exists as a non-directory;
  - `NotDirectory` when a component is a file;
  - `PermissionDenied`;
  - `NotSupported` (CloudBucketMount, irrelevant to wisp).
- Mode of new directories: UNVERIFIED, presumably 0755 under umask.

**Remove** — `{"Remove":{"path":P,"recursive":B}}`
- `recursive=false`: removes a file, a symlink (not its target) or an **empty** directory.
- `recursive=true`: like `rm -r`, which does not follow symlinks.
- stdout: ignored.
- Errors:
  - `NotFound`;
  - `DirectoryNotEmpty` for a non-empty directory with `recursive=false`;
  - `PermissionDenied`;
  - `NotSupported` (CloudBucketMount).

  `IsDirectory` is not mapped for Remove, so a plain-file `unlink` on a directory must be retried as `rmdir` rather
  than reported.

**Watch** — `{"Watch":{"path":P,"recursive":B,"filter":[...]|null,"timeout_secs":N|null}}`
- Behaviour (`sandbox_fs.py:465-557` and docstring):
  - If P is a file, the binary reports events for that file. If P is a directory, it reports events for its
    direct entries; `recursive=true` adds all nested subdirectories, including ones created later.
  - A symlinked P is followed; event paths are under the **resolved** target.
  - `filter=null` passes every event type. Otherwise `filter` is a list of event-type strings. The client expands
    `Modify` to also include `Rename`, `RenameFrom` and `RenameTo` (`sandbox_fs.py:50-65`).
- stdout: **one JSON object per line**, flushed per event:
  ```json
  {"event_type": "Create", "paths": ["/abs/path"]}
  ```
  - `event_type` ∈ `Unknown | Access | Create | Modify | Remove | Rename | RenameFrom | RenameTo`. The client maps
    the three rename variants to `Modify`.
  - `paths` is a non-empty list of absolute paths. Lines with empty or missing `paths` are skipped. Lines that are
    not JSON, lack `event_type`, or carry an unknown type are skipped silently.
  - Per the `FileWatchEvent` docstring (`types.py:346-357`), a rename with both ends inside the watched scope must
    be a single event with `paths: [source, destination]` (`Rename`). With one end visible it is a single path
    (`RenameFrom` / `RenameTo`).
- **Termination:**
  1. After `timeout_secs` the binary exits **0**. The client's iterator then ends cleanly.
  2. The binary must also exit (0) **when its stdin reaches EOF**. To stop the watch, the client does
     `stdin.write_eof(); stdin.drain()`, which is a `TaskExecStdinWrite` with `offset=0, data=b"", eof=True`. It
     then **waits for the exit** (`sandbox_fs.py:547-554`). A binary that ignores stdin EOF hangs the client
     forever.
  3. It ends when the sandbox terminates.
- Errors (exit before or while streaming):
  - `NotFound`;
  - `PermissionDenied`;
  - `NotSupported` (FUSE / CloudBucketMount);
  - inotify limits (`ENOSPC`) should map to some other kind and become a generic `SandboxFilesystemError`.
- The event names follow the Rust `notify` crate's `EventKind` (Access/Create/Modify/Remove/Other→Unknown) and
  `ModifyKind::Name(RenameMode::{Both,From,To})`. Notify's inotify backend watches `CREATE`, `DELETE`, `MODIFY`,
  `ATTRIB`, `MOVED_FROM`/`TO`, `CLOSE_WRITE`, `DELETE_SELF` and `MOVE_SELF`. Writing a file on hosted Modal
  therefore probably yields `Create`, `Modify` (data, perhaps more than once), then `Access` (`Close(Write)`), and a
  chmod yields `Modify` (Metadata). All UNVERIFIED.

### 3.5 FileInfo JSON (ListFiles items, Stat result)

`FileInfo(**fields)` is built by key lookup (`sandbox_fs.py:221-235,447-459`). A missing required key raises
`KeyError`, so the whole call fails.

| key | JSON type | Python field | Semantics / recommended value | Confidence |
|---|---|---|---|---|
| `name` | string | `name: str` | basename (`file_name()`); for `/`, probably `"/"` or `""` | name: certain; `/` case UNVERIFIED |
| `path` | string | `path: str` | absolute path: P for Stat, `P/<name>` for list entries; not canonicalised | likely |
| `type` | string | `FileType(...)` | **exactly** `"file"`, `"directory"` or `"symlink"`. Anything else raises `ValueError` in the client, failing the whole `list_files`. FIFOs, sockets and devices must be reported as `"file"`; listing `/dev` is the test case | enum: certain |
| `size` | int | `size: int` | `st_size` in bytes | certain |
| `mode` | int | `mode: int` | **UNVERIFIED** whether it is full `st_mode` (`0o100644` = 33188, Rust `PermissionsExt::mode()`) or permission bits only (420). Lean toward full `st_mode`, the Rust idiom given a separate `permissions` string | low |
| `permissions` | string | `permissions: str` | **UNVERIFIED** format: `"rw-r--r--"` (9 chars), `"-rw-r--r--"` (ls-style, 10 chars) or `"0644"`. Pick one and confirm against hosted Modal | low |
| `owner` | string | `owner: str` | user name of `st_uid`; fall back to the decimal uid | names: likely; fallback UNVERIFIED |
| `group` | string | `group: str` | group name of `st_gid`; fall back to the decimal gid | as above |
| `modified_time` | number | `modified_time: float` | `st_mtime` as Unix epoch seconds, fractional (the client keeps the number as is, not an ISO string) | type: certain (float annotation); epoch: likely |
| `symlink_target` | string or null, optional | `symlink_target: str \| None` | raw `readlink` target for symlinks, otherwise `null` or absent | certain (via `entry.get`) |

Unknown extra keys are ignored. Use `lstat` for both Stat and list entries.

### 3.6 Edge cases to test

- Path `/`.
- Paths with spaces, non-ASCII characters, or a trailing slash.
- A dangling symlink in Stat and ListFiles: Stat succeeds; `symlink_target` is set.
- ReadFile on a dangling symlink: `NotFound`.
- WriteFile through a symlinked parent.
- WriteFile over a read-only file: `PermissionDenied`. Root ignores the mode bits, but not on read-only mounts.
- Remove `/`. Hosted behaviour is UNVERIFIED; refuse it with `PermissionDenied` or generic.
- Watch for an event in a directory created after the watch started (recursive).
- Watch on a file that is then replaced through a rename (editors do this).
- A future `Command` variant (JSON key not recognised): exit 2 with
  `{"error_kind":"InvalidCommand","message":"unknown command"}`, which becomes a generic error.

### 3.7 Implementation sketch (wisp)

**Placement.** Use a static Go binary, `CGO_ENABLED=0`, about 500-700 lines. Two options:

1. A multicall mode of `cmd/sprite-env`, dispatched on `filepath.Base(os.Args[0]) == "modal-sandbox-fs-tools"`, the
   way `sudoMode()` already works (`W/cmd/sprite-env/main.go:66-70`, `sudo.go:33`).
2. Its own `cmd/modal-fs-tools`, added to `scripts/build-initrd.sh:12`.

**Install.** Extend `installTools` (`W/cmd/wisp-agent/init.go:90-117`) to place it at
`/__modal/.bin/modal-sandbox-fs-tools`. Do this only when a boot parameter marks a Modal VM, for example
`p["modal"]`, so ordinary sprites do not get a `/__modal`. Prefer a **tmpfs mounted at `/__modal`** with the binary
copied in, mode 0755 root:
- user images cannot shadow it;
- `snapshot_filesystem` / `snapshot_directory` will not capture it;
- it always matches the running wisp version.

The copy has to happen before the pivot, or from the agent after the pivot (re-exec `/proc/self/exe`).

**Mechanics.**
- ReadFile: `io.Copy` to stdout, with a size check first for `FileTooLarge`.
- WriteFile: `MkdirAll` the parent, then `O_CREAT|O_TRUNC|O_WRONLY` 0644, then copy from stdin. On error, write the
  JSON to stderr and `os.Exit(1)` without draining stdin.
- Watch: raw `unix.InotifyInit1`.
  - Pair `IN_MOVED_FROM` / `IN_MOVED_TO` by cookie into `Rename` (two paths), with unpaired halves as
    `RenameFrom` / `RenameTo`.
  - `IN_CLOSE_WRITE` → `Access`, `IN_ATTRIB` / `IN_MODIFY` → `Modify`, `IN_CREATE` → `Create`,
    `IN_DELETE` / `IN_DELETE_SELF` → `Remove`.
  - Add watches for new subdirectories when recursive.
  - Use a goroutine that waits on stdin EOF and an `os.Exit(0)` timer for `timeout_secs`.
  - Flush stdout after each line (unbuffered `os.Stdout.Write`).
- Error mapping from errno: `ENOENT`→NotFound; `EACCES`/`EPERM`/`EROFS`→PermissionDenied; `EISDIR`→IsDirectory;
  `ENOTDIR`→NotDirectory (`IsFile` for ListFiles on a file); `EEXIST`→AlreadyExists (Write) or PathAlreadyExists
  (MakeDirectory); `ENOTEMPTY`→DirectoryNotEmpty; `EFBIG` or the size limit→FileTooLarge; anything else→`"Io"`.

**Do not reuse wisp-agent's `/fs/*` HTTP API through a frontend intercept:**
- it `chown`s new paths to the `sprite` user (`W/internal/agent/fs.go:93-115`), whereas Modal's are root's;
- `/fs/list` on a file returns an entry;
- its error codes are too coarse: `bad_request` merges `EISDIR` and `ENOTDIR`, and `conflict` merges `EEXIST` and
  `ENOTEMPTY` (`fs.go:53-71`);
- `/fs/watch` uses fsnotify, whose rename comes out as `Rename(old)` + `Create(new)` rather than one two-path event
  (`fs_watch.go:51,327-365`).

An intercept would also have to fake exec semantics (stdin, wait, stderr) in the frontend. The in-guest binary
reuses the exec path that is already being built and tested.

---

## 4. Per-item notes

### 4.1 `sb.exec` → `TaskExecStart`

Client: `M/sandbox.py:2223-2292` (public) → `_exec` 2294-2347 → `_exec_through_command_router` 2349-2417.

- `exec_id` is a fresh `uuid4` and doubles as the idempotency key. Wisp handles this (`router.go:127-132`).
- `command_args = args`. Validation is client-side: all elements must be str, and the total length must be at most
  `ARG_MAX_BYTES` (`sandbox.py:266-275`).
- `stdout` / `stderr` map as follows (`sandbox.py:2370-2388`):
  - PIPE → `*_CONFIG_PIPE`;
  - DEVNULL → `*_CONFIG_DEVNULL`;
  - **`StreamType.STDOUT` is sent as PIPE**, and the client prints locally.

  So 1.6.0 never sends `TASK_EXEC_STDERR_CONFIG_STDOUT`. Wisp's `mergeStderr` (`router.go:226`) is harmless dead
  code for this client.
- `timeout_secs` is optional. The client also sets `exec_deadline = now + timeout`. Once it passes, `wait()` /
  `poll()` return **-1**, and stdio streams end silently on `ExecTimeoutError` (`container_process.py:154-185`,
  `io_streams.py:329-333`). Wisp kills the process and reports 137 (`router.go:147-150`), which the client usually
  never sees. That is fine.
- `workdir` must be absolute (checked client-side, `sandbox.py:2312`). For a missing directory, wisp's `env -C`
  exits 125 with a message on stderr; hosted Modal may reject the exec instead (UNVERIFIED, already noted in
  modal-differences.md).
- `env`: `None` values are dropped. Local secrets (`Secret.from_dict`) are merged into `env` client-side
  (`sandbox.py:2316-2324`); only named secrets populate `secret_ids`.
- `runtime_debug` comes from config and can be ignored.
- `container_id` is used for sidecars only.
- `text` and `bufsize` are client-side only. `bufsize=1` means line splitting and requires `text=True`
  (`io_streams.py:575-576`).
- No custom user: 1.6.0 has no `user=` parameter. Execs run as the image user (root). Wisp gets root through
  `sudo -n -H -- env -C dir K=V... argv` (`exec.go:33-55`). Keep that wrapper when moving to the WebSocket exec.
- Errors: `exec_start` retries transient gRPC errors 10 times with exponential backoff from 10 ms
  (`task_command_router_client.py:97-202,516-521`). `UNAUTHENTICATED` triggers one JWT refresh and a retry
  (`:921-930`).
- Wisp: done, apart from PTY, named secrets and `container_id`.

### 4.2 Stdout and stderr readers → `TaskExecStdioRead`

Client: `M/io_streams.py:311-476`; router client `:561-599,932-1082`.

- Request: `{task_id, exec_id, offset, file_descriptor: STDOUT=0|STDERR=1}`.
  `FILE_DESCRIPTOR_INFO` / `UNSPECIFIED` are rejected client-side.
- Response stream of `{data}`:
  - **an empty `data` raises `ValueError`** (`io_streams.py:325-326`);
  - the clean end of the stream means EOF;
  - offsets are tracked client-side.
- Resume: on retryable gRPC statuses (`DEADLINE_EXCEEDED`, `UNAVAILABLE`, `CANCELLED`, `INTERNAL`, `UNKNOWN`,
  `M/_utils/grpc_utils.py:198-204`), `StreamTerminatedError`, timeouts and `OSError`, the client reopens the
  stream at the current offset. The budget is 10 retries, with backoff from 10 ms ×2, reset after every successful
  chunk. `UNAUTHENTICATED` gets one refresh per attempt (`:1005-1012`).
- Idle release: when a consumer holds a chunk past `sandbox_channel_idle_timeout` (30 s), the client drops the
  connection and reopens at the offset (`:995-1017`). The server must therefore tolerate many short reads at
  arbitrary offsets. Wisp does (`router.go:243-275`).
- All iterators and `read()` calls on one reader share a single offset and lock (`io_streams.py:347-400`).
- Text mode uses an incremental strict UTF-8 decoder (`io_streams.py:432-472`), so invalid UTF-8 in output raises
  `UnicodeDecodeError` client-side, as it does on hosted Modal.
- Wisp gaps:
  - output past 64 MiB per stream is dropped silently (`router.go:46,59-64`). Prefer ending the stream with an
    error such as `RESOURCE_EXHAUSTED`, or document the limit;
  - all output is kept in memory until the sandbox ends (`stopExecs`), so a long-lived sandbox running many execs
    accumulates it. Consider evicting finished execs some minutes after their last read (UNVERIFIED how long
    hosted Modal keeps them).

### 4.3 `wait` and `poll` → `TaskExecWait` / `TaskExecPoll`

Client: `M/container_process.py:132-187`; router client `:801-883`.

- Response is a oneof `exit_status {code | signal}`. The client turns `signal` into `128+signal`; an unset oneof
  from Wait raises `InvalidError`.
- `TaskExecWait` uses a 60 s per-call timeout and is retried forever with a 1 s fixed delay. `NOT_FOUND` stops it.
- Wisp: done. Wisp reports signals as code `128+n`, which is equivalent.
- Failed execs map to `FAILED_PRECONDITION` (→ `ConflictError`) (`router.go:288-293`).

### 4.4 Exec stdin → `TaskExecStdinWrite` / `TaskExecStdinWriteStream` / `TaskExecStdinStatus`

Client: `M/io_streams.py:721-773`, `container_process.py:120-130`, router client `:636-768`.

**Writer semantics (`p.stdin`).**
- `write()` buffers locally, up to 16 MiB (`TASK_COMMAND_ROUTER_MAX_BUFFER_SIZE`, `io_streams.py:624`); beyond that
  it raises `BufferError`. `str` is encoded as UTF-8.
- `write_eof()` only sets a flag.
- `drain()` sends one `TaskExecStdinWrite{task_id, exec_id, offset, data, eof}` if the buffer is non-empty or the
  EOF flag is set. `offset` is the count of bytes acknowledged before this call. The buffer is cleared only after
  success.
- An empty drain without EOF sends nothing. The 5 s `b""` heartbeats in `attach()` therefore never reach the
  server.
- The client retries transient errors (10 times), so **the server must be idempotent by offset**:
  - `offset < written`: skip the overlap (a retried write) and accept the rest;
  - `offset > written`: a gap. Return an error, probably `FAILED_PRECONDITION` or `OUT_OF_RANGE` (UNVERIFIED).
- A second EOF is ignored ("the command router will ignore the second EOF", `io_streams.py:751`).
- `ConflictError` from this RPC propagates raw to the user for exec stdin. For V2 sandbox stdin it is turned into
  `ValueError`.

**Streaming (filesystem writes only, `_stdin_write_stream`).**
- A client-streaming `TaskExecStdinWriteStream`:
  - the first message is `start{task_id, exec_id, offset}`;
  - then `data` messages of up to 256 KiB each (empty chunks are skipped);
  - then `end{}`, which closes the half and **marks stdin closed**;
  - the response is empty.
- A stream that fails before `end` leaves stdin **open and resumable**.
- Resume:
  - On `AuthError`, `InternalError`, `ServiceError`, `StreamTerminatedError`, `OSError` or a timeout, the client
    calls `TaskExecStdinStatus` and gets `{num_bytes_written, closed}`.
  - If `closed` is set and everything was written, the response is treated as lost and the call returns success.
  - Otherwise it seeks the source to `num_bytes_written` and reopens.
  - It makes at most 9 attempts (`:657-713`).
- Per the docstring, `TaskExecStdinStatus` "evicts any in-flight stdin stream for the exec". The server must cancel
  any earlier stream still writing.
- If the process has exited (or closed stdin) while the client is still sending, the server must fail the stream
  with `FAILED_PRECONDITION` (→ `ConflictError`). The fs write path depends on this (`sandbox_fs.py:633-641`).
  `ConflictError` is not in the resumable set, so it propagates.

**Wisp mapping.**
- The agent can do stdin only on its exec **WebSocket** (`GET /exec?stdin=true`): frames `0x00`+data for stdin and
  `0x04` for EOF (`server.go:240-260`, `session.go:24-31`). `POST /exec` reads stdin from the request body
  (`server.go:302-316`) but the frontend sends `stdin=false` and `http.NoBody`.
- Plan: replace `agentExec` with the WebSocket client pattern from `W/frontend/daytona/agent.go:117-205`. Keep a
  per-execution stdin state:
  - `written` (bytes accepted), `closed`, and a goroutine that writes to the socket in order;
  - accept data into an in-memory queue capped at, say, 16-64 MiB, which gives backpressure by blocking the RPC;
  - answer the RPC when the bytes are queued (or forwarded);
  - `TaskExecStdinStatus` returns `{written, closed}`.
- Agent caveats:
  - `Session.WriteStdin` ignores write errors (`session.go:548-562`), so the frontend cannot see `EPIPE`. Use
    "execution finished" (`x.done`) or a failed socket write as the `FAILED_PRECONDITION` trigger.
  - In TTY mode `CloseStdin` is a no-op (`session.go:564-571`, `s.stdin` is nil). See 4.5.
  - The agent's WebSocket stdin write is synchronous inside the input goroutine. A process that never reads stdin
    fills the 64 KiB pipe and stalls the socket reader. That does not block output, which runs on a separate
    goroutine, but the frontend's socket writes will hit the 30 s write deadline. Keep the frontend queue
    non-blocking toward the gRPC caller and tolerate a stalled socket.
- Admin check: stdin RPCs should require `callerOf(ctx).admin`, as `TaskExecStart` does (`router.go:208`).
- Effort: **M**, covering the WebSocket switch plus three RPCs plus tests (resume, gap, duplicate, early exit).

### 4.5 PTY execs and `attach()`

Client:
- `M/sandbox.py:440-441` (`_default_pty_info`) and `2271-2279`;
- `M/_output/pty.py:31-42` (`get_pty_info`): `enabled=True`, `winsz_rows`/`cols` from the local terminal (0 when
  not a TTY), `env_term`/`env_colorterm`/`env_term_program` from the local env, `pty_type=SHELL(2)`;
  `no_terminate_on_idle_stdin=True` for `sb.exec`, False for `modal shell`'s `_container_exec`;
- `M/container_process.py:189-233` (`attach`).

Semantics:
- All output goes to stdout and stderr stays empty (docstring at `sandbox.py:2255-2257`). The client still opens a
  stderr `TaskExecStdioRead`, which must end cleanly at exit.
- **No resize RPC exists** in the 1.6.0 router service (the full list is in §2). The size is fixed at start, and
  `attach()` never sends SIGWINCH.
- Ctrl-C and similar keys travel as raw bytes in raw-terminal mode (`stream_from_stdin(..., use_raw_terminal=True)`,
  `M/_utils/shell_utils.py:87-124`). The PTY line discipline turns them into signals.
- `attach()` reads stdout and stderr, and gives up with `InteractiveTimeoutError` if no output arrives within 60 s.
  **A silent PTY program therefore fails to attach.** A shell prints a prompt, so this is normally fine.
- `modal shell <sb-id>` uses `_container_exec` (`sandbox.py:3316-3372`) with `text=False` for PTY. It needs
  `Sandbox.from_id` plus `SandboxGetTaskId[V2]`, and `SandboxGetCommandRouterAccess` (V2) or
  `TaskGetCommandRouterAccess` (V1).
- Why PTY is thought to force V1: only for **sandbox create**, and only through the deprecated `pty_info=`. Exec PTY
  works the same on V1 and V2 through `TaskExecStart.pty_info`.

Wisp mapping:
- Agent `GET /exec?tty=true&cols=C&rows=R&stdin=true`: `Session.Start` with a TTY (`session.go:318-334`).
  Rows/cols of 0 default to 80x24. The exit code arrives only as the JSON
  `{"type":"exit","exit_code":N}` (`session.go:496-504`). Daytona's `execConn.run` already parses it.
- Pass `TERM` (default `xterm-256color` when `env_term` is empty), `COLORTERM` and `TERM_PROGRAM` through the
  `env` part of `command()`.
- `sudo` inside a PTY works (it may allocate its own pty with `use_pty`).
- `write_eof()` on a PTY: the agent cannot close a PTY's input. Option: on EOF, send `\x04` (VEOF) once. Hosted
  behaviour is UNVERIFIED.
- `no_terminate_on_idle_stdin`: ignore it and never terminate on idle stdin. Hosted behaviour for
  `no_terminate_on_idle_stdin=False` (`modal shell`) is UNVERIFIED; with no heartbeat RPC it may be meaningless on
  the router path.
- Effort: **M**, mostly shared with the WebSocket switch. Once exec PTY works, `Sandbox.create(pty=True)` is **S**:
  run the entrypoint with a TTY.

### 4.6 Sandbox-level stdio (`sb.stdout`, `sb.stderr`, `sb.stdin`)

Client: `M/sandbox.py:1217-1291` (hydration), `2555-2589` (properties); `M/io_streams.py:34-206,286-308,659-790`.

**V2 (default).**
- `sb.stdout` and `sb.stderr` are `by_line=True` text readers over `SandboxStdioReadV2{task_id, offset,
  file_descriptor: SANDBOX_STDIO_FILE_DESCRIPTOR_STDOUT=0|STDERR=1}`. The stream yields
  `{data, starting_offset}`.
- On the **first chunk of each attempt** the client resets `offset = starting_offset`
  (`task_command_router_client.py:991-993`). If `starting_offset` is greater than the offset it asked for, it logs
  "dropped N bytes; only the most recent portion of output is retained" (`io_streams.py:300-306`). Hosted Modal
  therefore keeps a **bounded ring buffer** of entrypoint output; its size is UNVERIFIED.
- An empty `data` raises `ValueError`. There is no deadline, and the clean end of the stream is EOF.
- The router is resolved lazily (`resolve_router`): `_get_task_id()` and then
  `SandboxGetCommandRouterAccess(sandbox_id)`, unless access was seeded by `SandboxCreateV2`.
- `sb.stdin` sends `SandboxStdinWriteV2{task_id, offset, data, eof}` with the same buffer and offset rules as exec
  stdin (16 MiB). `ConflictError` becomes `ValueError`. `check_open` refuses use after `detach()`.

**V1 (`MODAL_SANDBOX_V2=0`, `sb-` IDs).**
- `sb.stdout` and `sb.stderr` read control-plane `SandboxGetLogs{sandbox_id, file_descriptor, timeout=55,
  last_entry_id}`, a server stream of `TaskLogsBatch`:
  - the client takes `batch.entry_id` as the resume cursor and yields `item.data` for each item;
  - `TaskLogs.data` is a **string** field;
  - `batch.eof=true` ends the stream (`io_streams.py:34-50,128-171`);
  - empty `data` items are skipped;
  - `ServiceError`/`InternalError` retry after 1 s, and `StreamTerminatedError` retries at once, 10 times.
- Bytes-mode readers are refused for V1 sandbox streams (`io_streams.py:97-98`).
- **Go gotcha:** protobuf-go refuses to marshal invalid UTF-8 in a `string` field. Entrypoint output must be cut at
  rune boundaries, with invalid bytes replaced by U+FFFD, before it goes into `TaskLogs.data`.
- `sb.stdin` (V1) sends `SandboxStdinWrite{sandbox_id, input, index, eof}`. `index` starts at 1 and increments on
  **every** `drain()`, even empty ones (`io_streams.py:659-718`). The server should deduplicate by index.
  `ConflictError` becomes `ValueError`. The buffer limit is 2 MiB.

**Wisp mapping and gaps.**
- The entrypoint runs through `startExec(..., "")` with `stdout/stderr=false`, so its **output is discarded**
  (`control.go:333-353`, `router.go:96-107`). It also has no stdin (`/dev/null`). Hosted gives the entrypoint an
  open stdin pipe, so an entrypoint of `cat` waits there, while on wisp it exits immediately. That is a behaviour
  difference worth fixing along with the WebSocket switch.
- Keep both streams in a ring buffer (for example 1-16 MiB per stream, discarding the oldest data) with absolute
  offsets. `SandboxStdioReadV2` serves from `max(offset, ring_start)` with `starting_offset` set accordingly.
  `SandboxGetLogs` serves the same buffer with `entry_id = decimal end-offset`.
- **Post-exit reads:** a common pattern is `sb = Sandbox.create("bash","-c","echo hi"); sb.wait();
  print(sb.stdout.read())`. Supporting it needs all of the following:
  - `SandboxGetCommandRouterAccess` / `TaskGetCommandRouterAccess` to answer for **finished** sandboxes. Today
    `runningSandbox` refuses them (`control.go:506,514`);
  - `forTask` to let `SandboxStdioReadV2` through after the result is set (`router.go:184-186`);
  - the buffer to survive `stopExecs` and the VM stopping, kept for some retention period.
- Effort: **M** for V2 read/write plus post-exit support. Another **M** for V1 `SandboxGetLogs` and
  `SandboxStdinWrite`, or skip V1 sandbox stdio and document that.

### 4.7 JWT, retries, error mapping (all router calls)

- JWT:
  - The client parses `exp` only, sends `authorization: Bearer <jwt>`, and on `UNAUTHENTICATED` refreshes through
    `TaskGetCommandRouterAccess` (V1) or `SandboxGetCommandRouterAccess` (V2). It skips the refresh if `exp` is
    more than 30 s away (`task_command_router_client.py:885-930`).
  - Wisp issues 1-hour HS256 tokens and returns `UNAUTHENTICATED` on expiry (`meta.go:103-145`,
    `modal.go:189-199`), so refresh works, as long as the access RPCs answer.
- gRPC status → exception (`M/_grpc_client.py:27-44`):
  - `FAILED_PRECONDITION`/`ABORTED` → `ConflictError`
  - `NOT_FOUND` → `NotFoundError`
  - `INVALID_ARGUMENT`/`OUT_OF_RANGE` → `InvalidError`
  - `UNIMPLEMENTED` → `UnimplementedError`
  - `UNAVAILABLE`/`CANCELLED`/`UNKNOWN`/`DEADLINE_EXCEEDED` → `ServiceError`
  - `INTERNAL` → `InternalError`
  - `UNAUTHENTICATED` → `AuthError`
- Retryable codes: `DEADLINE_EXCEEDED`, `UNAVAILABLE`, `CANCELLED`, `INTERNAL`, `UNKNOWN`.
- **Do not return `INTERNAL` or `UNKNOWN` for permanent errors from stdio or stdin RPCs.** The client retries them
  10 times with backoff before giving up.
- A missing RPC (not registered) is answered `UNIMPLEMENTED` by grpc-go, which becomes `UnimplementedError`. That
  is a clean failure.

---

## 5. Decisions for the owner

1. **Where fs-tools lives.** The recommendation is an in-guest static binary. Choose between:
   - a `sprite-env` multicall or a separate `cmd/`;
   - installing on disk or on a tmpfs at `/__modal`. The tmpfs keeps it out of snapshots and away from user
     images.

   The alternative, emulating fs-tools in the frontend on top of agent `/fs/*`, is weaker: wrong ownership, coarse
   errors and fsnotify renames.
2. **Gate on Modal.** Install `/__modal` only for VMs created by the Modal frontend (boot parameter), or for every
   sprite.
3. **FileInfo formats** for `mode` (full `st_mode` or permission bits) and `permissions` (string style). Pick now
   and confirm against hosted Modal; see §6.
4. **Read size limit:** set `FileTooLarge` at or below the frontend's per-stream cap, or raise the cap. Also decide
   whether exec output past the cap should become an error instead of being truncated silently.
5. **Exec transport:** move every Modal exec, entrypoint included, from `POST /exec` to the agent WebSocket with
   `stdin=true`. This changes behaviour: commands that read stdin now block, as on hosted Modal. The alternative is
   to open stdin only when a stdin RPC first arrives, which is not possible because the pipe has to exist at
   start. Recommended: always open.
6. **Stdin buffering and backpressure:** how much the frontend queues per exec before a `TaskExecStdinWrite` blocks,
   and which status a gap or overlap returns.
7. **Entrypoint stdio retention:** ring size, and how long a finished sandbox's stdio and router access stay
   available.
8. **V1 sandbox stdio** (`SandboxGetLogs` / `SandboxStdinWrite`): implement it, or document it as unsupported
   because V1 is opt-in.
9. **PTY EOF:** whether `write_eof()` on a PTY exec sends `^D` or nothing.
10. **Correct the docs:** `docs/providers/modal.md:25,163` claims PTY forces V1. In 1.6.0 that is true only for the
    deprecated `pty_info=` on create.

---

## 6. Unverifiable without hosted Modal

- **fs-tools:**
  - exact `message` text per error, and the full set of `error_kind` strings the Rust crate can emit;
  - `mode` and `permissions` formats, owner/group fallback for unknown IDs, `name` for `/`;
  - ListFiles ordering and whether a symlinked directory argument is followed;
  - the read size limit for `FileTooLarge`;
  - whether WriteFile is atomic; new-file mode and owner; whether an overwrite keeps the mode and owner;
  - the exit code used for errors (only non-zero matters);
  - the exact watch event stream for common operations (write, rename, chmod, delete of the watched directory):
    whether `Access` appears for `CLOSE_WRITE` and whether `Unknown` is ever emitted;
  - watch behaviour when the watched path itself is removed: does it exit, and with what code?
- **Exec:**
  - hosted handling of a non-existent `workdir` (rejected at `TaskExecStart` or exit code);
  - the status code for a stdin offset gap or overlap, and for a stdin write after exit (the docs say
    `ConflictError`; `FAILED_PRECONDITION` or `ABORTED` is UNVERIFIED);
  - whether `TaskExecWait` returns `signal` or `code` for signalled processes;
  - how long finished exec output stays readable;
  - per-stream output caps.
- **PTY:**
  - `write_eof()` semantics;
  - what `no_terminate_on_idle_stdin=False` does on the router path;
  - default TERM when `env_term` is empty;
  - whether stderr is really always empty.
- **Sandbox stdio:**
  - ring-buffer size;
  - post-exit retention period;
  - `SandboxGetCommandRouterAccess` behaviour for finished sandboxes;
  - V1 `entry_id` format (it is opaque, so any value works).

## 7. Key file references

- Client:
  - `M/sandbox_fs.py`
  - `M/_utils/sandbox_fs_utils.py`
  - `M/types.py:39-55,318-357`
  - `M/container_process.py`
  - `M/io_streams.py`
  - `M/_utils/task_command_router_client.py`
  - `M/sandbox.py:440,851-853,1065-1068,1217-1291,2148-2417,2547-2598,3316-3372`
  - `M/_output/pty.py:31-42`
  - `M/_grpc_client.py:27-44`
- Wisp today:
  - `W/frontend/modal/router.go` (exec RPCs, buffers)
  - `W/frontend/modal/exec.go` (`agentExec` via `POST /exec`, `stdin=false`)
  - `W/frontend/modal/control.go:236,333-353,501-519`
  - `W/frontend/modal/modalpb/prune.py:18-44` (KEEP list to extend)
- Agent:
  - `W/internal/agent/server.go:92-115,240-283,287-368`
  - `W/internal/agent/session.go:24-37,277-383,496-571`
  - `W/internal/agent/fs.go`
  - `W/internal/agent/fs_watch.go`
- Shipping precedent: `W/cmd/wisp-agent/init.go:84-158`, `W/cmd/sprite-env/main.go:66-70`,
  `W/scripts/build-initrd.sh:12`.
- WebSocket exec client to copy: `W/frontend/daytona/agent.go:117-205`.
