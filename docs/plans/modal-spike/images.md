# Modal images on wisp sandboxd: what modal 1.6.0 sends, and how to build it for real

Spike area: **images**. This is read-only research; the wisp repo is untouched. Client: PyPI `modal==1.6.0`
(`M=.../modal16/.venv/lib/python3.14/site-packages/modal`). Every request shape below was **captured**
from the unmodified client against a recording fake server (`spike/cap/fakeserver.py`, a grpclib
`ModalClientBase` that logs every RPC as protobuf text. Drivers are `spike/cap/client*.py`, and the raw
logs are `spike/cap/cap*.log`). Line numbers are in `$M/_image.py` unless another file is named.

---

## 1. Summary

- **Every Image API comes down to one RPC:** `ImageGetOrCreate(Image{base_images, dockerfile_commands,
  context_files, context_mount_id, secret_ids, gpu_config, image_registry_config, build_args, volume_mounts,
  build_function})`. The client turns each builder method into Dockerfile text (`FROM base` + instructions),
  so **a real Modal server is a Dockerfile builder**. The builder also needs a few side channels:
  - `Mount*` and `Blob*` RPCs plus an HTTP PUT endpoint, for `COPY` sources and `add_local_*`.
  - `SecretGetOrCreate`, for `env=`, `secrets=` and registry credentials.
  - A **global mount** named `python-build-standalone.<rel>.<ver>-gnu`, for `add_python=`.
- **Layering is client-driven.** Each chained method is a separate image whose `base_images=[{docker_tag:"base",
  image_id:<parent>}]`. The client re-sends `ImageGetOrCreate` for **every layer on every run**. Steady state
  is therefore N cache lookups, each answered `result.status=SUCCESS` immediately. Only a miss returns no status,
  and the client then follows the build with `ImageJoinStreaming` (a resumable log stream, ending with a
  `GenericResult` plus `ImageMetadata`).
- **Image IDs should be deterministic in the recipe.** A child's definition embeds its parent's ID, so stable
  IDs give layer reuse for free. The client never compares IDs itself.
- **The default sandbox image is a build recipe.** `Sandbox.create(image=None)` uses `Image.debian_slim()`
  (`sandbox.py:82`). Its `FROM python:<local-series-pinned>-slim-bookworm` changes with the caller's local
  Python (3.10.17 / 3.11.12 / 3.12.10 / 3.13.3 / 3.14.2 on builder 2025.06). In practice almost every Modal
  program also chains `.pip_install`, `.apt_install` or `.run_commands`. So **running RUN steps is the core
  requirement, not an edge case.** This goes beyond what the peer front ends needed: E2B templates and Daytona
  `buildInfo` are also unsupported (`docs/providers/*-differences.md`).
- **Recommendation: a hybrid native builder.**
  - The host does what runs no user code: it pulls the `FROM` image with the existing `engine.ImageCache`
    (podman pull → ext4) and extracts `COPY --from=<image>` files.
  - Every `RUN` executes **inside a Firecracker builder sprite** cloned from the parent layer's disk, driven
    through wisp-agent exec/fs.
  - The result is committed with the same quiesce + reflink clone that checkpoints use, into an image store
    keyed by `im-` ID.
  - Layers then cost only changed blocks, isolation is a VM with wisp's egress policy, and almost every piece
    already exists.
  - Rejected: host podman/buildah building, which runs friends' untrusted RUN on the host kernel and network as
    the user that owns every VM disk. Kept for later: buildah inside a builder VM, as a fallback for complex
    multi-stage Dockerfiles.
- **One verified trap for public deployments.** Single-part blob uploads call `use_md5(url)`
  (`_utils/blob_utils.py:728`), which **raises "Unknown S3 host"** unless the URL host is `localhost`, a
  private/loopback IP, `*.amazonaws.com` or `*.r2.cloudflarestorage.com`. Answer every `BlobCreate` with
  `multiparts` instead: part PUTs carry no MD5, so `use_md5` is never called. This was verified with `use_md5`
  patched to raise (`spike/cap/client3.py`, `cap4.log`).
- **`run_function` is out of scope.** It needs the whole Functions runtime: FunctionCreate with
  `is_builder_function`, the modal client mount in the container, and the input/output RPCs.

---

## 2. Table: API → what is sent → RPCs → effort

Builder `2025.06` (what wisp reports, `frontend/modal/modal.go:63`). All requests have `app_id`,
`builder_version:"2025.06"`, `namespace` (GLOBAL for debian_slim/micromamba/from_scratch, WORKSPACE otherwise),
and empty `gpu_config{}` / `image_registry_config{}` unless stated. Effort is for a wisp-familiar engineer:
S ≤ 1 day, M 2–4 days, L 1–2 weeks. Each row assumes the shared core (builder VM + store + log stream).

| API (`_image.py` line) | dockerfile_commands sent (captured) | Other fields | RPCs | Effort |
|---|---|---|---|---|
| `debian_slim(py=None)` :2577 | `FROM python:3.14.2-slim-bookworm`, `RUN apt-get update`, `RUN apt-get install -y gcc gfortran build-essential`, `RUN pip install --upgrade pip wheel uv`, `RUN echo 'debconf debconf/frontend select Noninteractive' \| debconf-set-selections`, `CMD ["sleep", "172800"]` | ns GLOBAL | EnvironmentGetOrCreate (builder version), ImageGetOrCreate, [ImageJoinStreaming] | S on the core; prewarm 5 versions |
| `debian_slim("3.12")` / `("3.11.9")` | `FROM python:3.12.10-slim-bookworm` / `FROM python:3.11.9-slim-bookworm` (micro passed through verbatim), rest identical | | same | (same) |
| `micromamba("3.11")` :2070 | `FROM mambaorg/micromamba:2.1.1-debian12-slim`, `SHELL ["/usr/local/bin/_dockerfile_shell.sh"]`, `ENV MAMBA_DOCKERFILE_ACTIVATE=1`, `RUN micromamba install -n base -y python=3.11 pip -c conda-forge`, `CMD ["sleep", "172800"]` | ns GLOBAL | same | S (needs SHELL + image ENV honoured) |
| `.micromamba_install(...)` :2111 | `FROM base`, `RUN micromamba install numpy -c conda-forge --yes` (+`COPY /<spec> /<spec>` with spec_file) | base_images, context_files for spec_file | same | S |
| `from_registry(tag)` :2218 | `FROM ubuntu:24.04` (+ `setup_dockerfile_commands` verbatim) | | same | S (ImageCache exists) |
| `from_registry(..., add_python="3.12")` | `FROM ubuntu:24.04`, `COPY /python/. /usr/local`, `RUN ln -s /usr/local/bin/python3 /usr/local/bin/python` (only for <3.13), `ENV TERMINFO_DIRS=/etc/terminfo:/lib/terminfo:/usr/share/terminfo:/usr/lib/terminfo`, then setup cmds | `context_mount_id` = global mount | **MountGetOrCreate{deployment_name:"python-build-standalone.20240107.3.12.1-gnu", namespace:GLOBAL}** (lookup, no files) | M (fetch + pin python-build-standalone) |
| `from_registry(tag, secret=)` | `FROM ghcr.io/me/priv:1` | `image_registry_config{REGISTRY_AUTH_TYPE_STATIC_CREDS, secret_id}` | SecretGetOrCreate (`env_dict`, ANONYMOUS_OWNED_BY_APP), ImageGetOrCreate | M (secret store + per-pull creds) |
| `from_aws_ecr` :2354 | `FROM 123.dkr.ecr...amazonaws.com/x:1` | `REGISTRY_AUTH_TYPE_AWS` + secret | same | M (ECR GetAuthorizationToken, SigV4) |
| `from_gcp_artifact_registry` :2293 | `FROM us-docker.pkg.dev/p/r/x:1` | `REGISTRY_AUTH_TYPE_GCP` + secret | same | S–M (`_json_key` basic auth) |
| `from_scratch()` :2542 | `FROM scratch` | ns GLOBAL | same | S (empty disk. Useless as a sandbox: no shell) |
| `from_dockerfile(path, context_dir, build_args, add_python)` :2415 | Layer 1: the **file's lines split on `\n`, verbatim**, including comments, continuation fragments and a trailing `""`. Layer 2: `FROM base` alone (or the add_python COPY/RUN/ENV trio) | L1: `context_mount_id` (files matched by COPY patterns, `.dockerignore` applied client-side), `build_args`. L2: base_images (+ python mount) | MountBatchedCheckExistence, MountPutFile, BlobCreate + HTTP PUT for ≥4 MiB, MountGetOrCreate, ImageGetOrCreate ×2 | M single-stage; L multi-stage |
| `.pip_install(*pkgs, index_url, pre, extra_options, …)` :1115 | `FROM base`, `RUN python -m pip install numpy==2.0 requests --index-url https://pypi.org/simple --pre` (pkgs **sorted**, shlex-quoted) | secrets/env/gpu/force_build as given | ImageGetOrCreate | (core) |
| `.pip_install_private_repos` :1202 | `RUN bash -c "[[ -v GITHUB_TOKEN ]] \|\| (…exit 1)"`, `RUN apt-get update && apt-get install -y git`, `RUN python3 -m pip install "git+https://user:$GITHUB_TOKEN@github.com/…"` | secret_ids required | + SecretGetOrCreate | (core + secrets) |
| `.pip_install_from_requirements(f)` :1323 | `FROM base`, `COPY /.requirements.txt /.requirements.txt`, `RUN python -m pip install -r /.requirements.txt` | `context_files[{filename:"/.requirements.txt", data:<bytes>}]` | ImageGetOrCreate | S |
| `.pip_install_from_pyproject(f, optional_dependencies)` :1385 | `RUN python -m pip install 'httpx>=0.27' pytest rich` (deps parsed **client-side**, sorted) | | ImageGetOrCreate | (core) |
| `.uv_pip_install(*pkgs, requirements=[…])` :1469 | `COPY --from=ghcr.io/astral-sh/uv:latest /uv /.uv/uv`, `COPY /.0_requirements.txt /.uv/0/requirements.txt`, `RUN /.uv/uv pip install --python $(command -v python) --compile-bytecode --requirements /.uv/0/requirements.txt requests` | context_files `/.<i>_<basename>` | ImageGetOrCreate | M (`COPY --from=<image>`) |
| `.poetry_install_from_file(pyproject, lock)` :1601 | `RUN python -m pip install poetry`, `COPY /.poetry.lock /tmp/poetry/poetry.lock`, `COPY /.pyproject.toml /tmp/poetry/pyproject.toml`, **`RUN cd /tmp/poetry && \ `** / **`  poetry config virtualenvs.create false && \ `** / `poetry install --no-root --compile` (three list items that only parse as one continued RUN) | context_files | ImageGetOrCreate | S (needs a real Dockerfile parser) |
| `.uv_sync(dir)` :1711 | `COPY --from=ghcr.io/astral-sh/uv:latest /uv /.uv/uv`, `COPY /.pyproject.toml /.uv/pyproject.toml`, `COPY /.uv.lock /.uv/uv.lock`, `RUN /.uv/uv sync --project=/.uv --no-install-workspace --compile-bytecode --frozen`, `ENV PATH=/.uv/.venv/bin:$PATH` | context_files | ImageGetOrCreate | S (on top of COPY --from) |
| `.apt_install(*pkgs)` :2643 | `FROM base`, `RUN apt-get update`, `RUN apt-get install -y git curl` (not sorted) | | ImageGetOrCreate | (core) |
| `.run_commands(*cmds, env, secrets, gpu, volumes)` :2026 | `FROM base`, `RUN echo a`, `RUN echo b` | `secret_ids:[st-3, st-4]` (env= becomes an extra **anonymous secret**), `gpu_config{count:1, gpu_type:"T4"}`, `volume_mounts` if volumes= | + SecretGetOrCreate ×N | (core). Volumes: depends on the volumes area |
| `.env({...})` :2814 | `FROM base`, `ENV X='a b'` (shlex-quoted) | | ImageGetOrCreate (its own layer) | S (metadata) |
| `.workdir(p)` :2845 | `FROM base`, `WORKDIR /w` | | own layer | S |
| `.entrypoint([...])` :1984, `.cmd([...])` :2875, `.shell([...])` :2005 | `ENTRYPOINT ["/bin/sh", "-c"]` / `CMD ["sleep", "9"]` / `SHELL ["/bin/bash", "-c"]`, each via `dockerfile_commands` (own layer) | context mount fn runs, but there are no COPY patterns, so no mount | own layer each | S |
| `.dockerfile_commands(*cmds, context_files, context_dir, build_args, secrets, gpu)` :1895 | `FROM base` + cmds verbatim (e.g. `ARG V=1`, `COPY sub/a.txt /a.txt`, `RUN cat /a.txt`) | `build_args{V:2}`, `context_mount_id` (files matching COPY patterns relative to context_dir). **context_files and an (often empty) context mount can both be present** | Mount* then ImageGetOrCreate | M |
| `.add_local_file/dir/python_source(copy=True)` :845/:882/:961 → `_copy_mount` :827 | `FROM base`, `COPY . /` | `context_mount_id` = mount whose files are at their **final absolute paths** (`/d/data.json`, `/s/big.bin`, `/root/mypkg/__init__.py`), `mode` (e.g. 436 = 0664) | Mount*, Blob* for ≥4 MiB | M |
| `.add_local_*(copy=False)` (default) :557 | **No image layer.** Same image_id as the parent. The mount is attached at sandbox create: `Sandbox.definition.mount_ids=[mo-…, …]` (`sandbox.py:560,1133`) | | Mount* at build, then SandboxCreateV2 with `mount_ids` | M (place files in VM before the entrypoint) |
| `Image.from_id(id)` :1016 | none | | **ImageFromId** on hydrate (lazy: only when used) → `{image_id, metadata}` | S |
| `img.publish(name)` :3026 / `Image.from_name(name)` :2987 | none | tag `name:latest` (`:tag` default), `env/` prefix optional | **ImagePublish**`{image_id, tag, allow_public}` → `{image_id, revision_id}`. **ImageGetByTag**`{tag}` → `{image_id}` (must be NOT_FOUND for unknown) | S |
| `img.logs.fetch/tail` :3100, `modal image logs` | none | | **ImageBuildChainGet** → `build_steps[{image_id, started_at, finished_at, builder_app_id, builder_task_id}]`, then **AppFetchLogs/AppCountLogs** filtered by task | M (low priority) |
| `modal image list/…` CLI | | | ImageListTags, ImageTagRevisions, ImageDelete (`experimental/__init__.py:257`) | S (low priority) |
| `run_function(f, …)` :2695 | none: `build_function{definition, globals, input}` + `build_function_id` | | FunctionCreate (builder fn) + the whole function runtime in the build container | **Out of scope** (weeks) |
| `force_build=True` / `MODAL_FORCE_BUILD` | unchanged | `ImageGetOrCreateRequest.force_build:true`. The client propagates it to all **children** (`self.force_build or force_build`) | | S |
| `MODAL_IGNORE_CACHE` | unchanged | `ignore_cache:true` | | S |
| `imports()`, `pipe()`, `hydrate()`, `build(app)` | client-only (`build` = resolver load + `_failed_build_attempt` bookkeeping, :1041) | | | none |

Client-side only, so nothing for the server: package sorting and quoting, pyproject parsing, `.dockerignore`
and COPY-pattern filtering of context mounts, Python series validation, and the "build step after
`add_local_*`" error (`_assert_no_mount_layers`, :587).

---

## 3. Protocol details the server must get right

### 3.1 ImageGetOrCreate / ImageJoinStreaming (`_from_args` :607–825, `_image_await_build_result` :448–480)
- **Response.** The response is `{image_id, result: GenericResult, metadata: ImageMetadata}`.
  - **If `result.status != 0`**, the client treats the image as finished and does no join (:769). Return
    `SUCCESS` plus metadata for cache hits.
  - **If status is 0 (unset)**, the client calls `ImageJoinStreaming{image_id, timeout:55, last_entry_id}` in a
    loop until a response carries `result.status`. It keeps reading after the result until the stream ends,
    so logs can trail the result.
  - **Retries.** Each stream end without a result is simply re-requested with the last `entry_id`. Only
    `ServiceError`, `InternalError` or `StreamTerminatedError` count toward the 3-retry limit. The server
    should end each stream by about 55 s (proxies) and **resume from `last_entry_id` without re-sending**.
    `eof` is ignored by this client.
- **Log entries.** Logs travel in `task_logs[]: TaskLogs{data, file_descriptor (STDOUT=1/STDERR=2/INFO=3)}`.
  An entry with `task_progress.pos/len` set must be of type `IMAGE_SNAPSHOT_UPLOAD` (an assert in the client).
  It is optional and can be skipped.
- **Failure mapping** (:782–809):
  - `FAILURE` (or `MEMORY_MANAGER_EVICTION`) with `exception` → `ImageBuildError("Image build for im-… failed with the exception:\n<exception>")`.
    Without `exception`, the message points to `modal image logs <id>`. Captured: `exception:"Build step failed: exit 1"` came out verbatim.
  - `TERMINATED` → "terminated due to external shut-down".
  - `TIMEOUT` → "timed out".
  - Any other status → "Unknown status".
  - A gRPC error from `ImageGetOrCreate` itself (what wisp does today: FAILED_PRECONDITION) also surfaces, but
    as a generic error and not `ImageBuildError`. Prefer `result FAILURE` with a readable `exception`.
- **Metadata.** `ImageMetadata` has optional `python_version_info`, `python_packages`, `workdir`,
  `libc_version_info` and `image_builder_version`. Sandboxes need none of them; set at least
  `image_builder_version` and `workdir`.
- **Timeouts.** `ImageGetOrCreate` has no per-attempt timeout by default (`_grpc_client.py:73`, `Retry()`),
  but builds should still be asynchronous so logs can stream.
- **`existing_image_id`** is marked "TODO: ignored" in the client (:754). Ignore it.

### 3.2 Mounts and blobs (`mount.py:475–683`, `_utils/blob_utils.py`)
1. The client sends `MountBatchedCheckExistence{sha256_hex_hashes[]}` in batches of 64 and expects
   `{missing_sha256_hex_hashes}` back.
2. For each missing file:
   - **Under 4 MiB** (`LARGE_FILE_LIMIT`, blob_utils:43): `MountPutFile{sha256_hex, data}`. The client loops
     until `exists:true`, for up to 10 minutes.
   - **4 MiB or more:** `BlobCreate{content_md5(b64), content_sha256_base64, content_length}`. The response
     must set `blob_ids[]` and **either** `upload_urls{items[]}` **or** `multiparts{items[MultiPartUpload{part_length, upload_urls[], completion_url}]}`.
     The deprecated singular `upload_url`/`multipart` are not read.
     - Single part: HTTP PUT with `Content-MD5` (only if `use_md5(host)`). The response needs an `ETag` equal
       to the md5 hex.
     - Multipart: one PUT per part (no MD5, no content type), each answered with `ETag: "<md5 hex>"`. Then a
       POST of `<CompleteMultipartUpload>…` to `completion_url`, whose **response body must contain**
       `md5(concat(part md5 bytes)).hex()-N`.
     - After the upload: `MountPutFile{sha256_hex, data_blob_id}`.
3. Finally `MountGetOrCreate{object_creation_type: ANONYMOUS_OWNED_BY_APP, app_id, files[{filename (absolute), sha256_hex, mode}]}`
   → `{mount_id, handle_metadata{content_checksum_sha256_hex}}`. The mount ID should be content-derived so
   image IDs stay stable.
4. **Global mounts are looked up by name:** `MountGetOrCreate{deployment_name, namespace: GLOBAL}` with no files
   and no creation type. Today only the python-build-standalone mounts reach this path. The names come from
   `mount.py:45–86`:
   - `3.10` → 20230826 / 3.10.13
   - `3.11` → 20230826 / 3.11.5
   - `3.12` → 20240107 / 3.12.1
   - `3.13` → 20241008 / 3.13.0
   - `3.14` → 20251205 / 3.14.2
   - `3.14t` → 20251209 / 3.14.2t

   The mount must contain `/python/...`: `COPY /python/. /usr/local` uses it. That is the layout of
   python-build-standalone's `*-install_only` tarballs.
5. **Public-host trap.** `use_md5` raises for hosts that are not localhost, private IPs or AWS/R2. Always answer
   `multiparts`; the comment at blob_utils:51 says the server may request multipart for small files. Blob URLs
   need their own auth, such as an HMAC-signed path with an expiry. The client sends no Modal headers on these
   PUTs.

### 3.3 Context resolution (what a COPY source means)
- **Sources.** The build context of one layer is the union of two things:
  - `context_files`: absolute names such as `/.requirements.txt`, with inline bytes.
  - `context_mount_id`: files at their mount paths.
- **Paths.** COPY sources resolve against the context root; a leading `/` means the root (:361).
- **Two mount shapes:**
  - `copy=True` mounts hold files at their final absolute paths, with `COPY . /`.
  - Dockerfile and `dockerfile_commands` context mounts hold files relative to `context_dir`
    (`/sub/a.txt` for `COPY sub/a.txt …`).
- **Modes.** Each file carries `mode`. Docker semantics give root:root ownership unless `--chown`.

### 3.4 Determinism and caching
- **Base layers are re-sent every run.** In the capture, `debian_slim` was sent before every chained case.
  With equal recipes, the child `base_images.image_id` stays `im-415db…` everywhere. A server that minted
  random IDs would rebuild every child every run.
- **Recommended ID:** `im-` + hash over:
  - `builder_version`
  - the canonical `Image` minus volatile fields: `base_images` IDs, `dockerfile_commands`, the
    `context_files` contents, the `context_mount` *content* checksum, `build_args`, `image_registry_config`,
    `gpu_config`
  - a **content hash of the secrets** (not their IDs; see §8)
- **`force_build`.** Rebuild, and give the result a **new** ID (recipe hash + generation). Point the recipe
  index at it so children rebuild, as the client docs describe for force_build: "will break the cache for all
  images based on the rebuilt layers".
- **`ignore_cache`.** Build under a new ID and do **not** update the recipe index.
- **Failed builds.** Do not cache them as the recipe's image; the next request rebuilds. Keep the failed ID's
  logs queryable (for `include_logs_for_finished` and `modal image logs`).
- **Concurrency.** Concurrent requests for the same recipe join one build. `ImageCache.Pull`'s flight map is
  the pattern (`engine/images.go:237`).

### 3.5 Sandbox-side consequences (`frontend/modal/control.go`)
- **Image lookup.** `SandboxCreate*` must map `definition.image_id` to *that image's* disk. Today every image
  maps to `f.opts.Disk` (control.go:252–256, :306).
- **Runtime mounts.** `definition.mount_ids` must be materialised: write the files into the VM before the
  entrypoint. Today they are refused at control.go:239.
- **Image metadata at runtime:**
  - `ENV` belongs in every exec's env. `command()` in exec.go:33 passes env explicitly through sudo, so
    image ENV must be merged there.
  - `WORKDIR` is the default workdir (modal-differences notes `/` today).
  - With no args, the entrypoint is image `ENTRYPOINT`+`CMD`. debian_slim's `CMD ["sleep","172800"]` exists
    precisely so a sandbox without args stays up (:2627).
- **`ImageFromId`** must answer for any built image, including sandbox snapshot images. Those come from
  another area, but should land in the same store.

---

## 4. Python version selection and base images

- **Version from the client.** `python_version=None` means the **client's local** `major.minor`, mapped to the
  pinned micro in `builder/base-images.json["python"]["2025.06"]` (:206–228):
  `3.10.17, 3.11.12, 3.12.10, 3.13.3, 3.14.2`. Supported series for 2025.06 are
  `3.10–3.14` plus `3.14t` (:78–84). debian_slim refuses free-threaded; `add_python` allows it.
- **Micro versions.** An explicit micro (`"3.11.9"`) is passed through verbatim into the tag.
- **Debian codename** is `bookworm` for 2025.06.
- **How to serve them.** Treat `FROM python:3.X.Y-slim-bookworm` like any `FROM`: normalise it to
  `docker.io/library/python:…` (`ociimage.ParseRef`) and use `engine.ImageCache.Acquire(pull=true)`. That
  path already pulls, flattens, adds the `sprite` account and the stand-in sudo, and caches by podman image ID.
  The debian_slim RUN steps then run in the builder like any other layer.
- **Prewarm.** Ship an operator command, an analogue of `make images` / `wispd images pull`, that builds the 5
  canonical debian_slim recipes ahead of time. A cold miss costs a Docker Hub pull plus apt
  build-essential, about 1–3 min, which collides with users' first `Sandbox.create`.
  - Each version is roughly 150 MB of python-slim plus about 450 MB of build-essential: **about 3 GB for all
    five**, less with reflink sharing.
  - Pre-pulling also dodges Docker Hub's anonymous rate limits.
- **Existing prebuilt disk.** `images/modal/Containerfile` can be dropped, or imported once as the cached
  result of the 3.14 recipe. Its extras (sudo, procps, iproute2) are not what hosted debian_slim has. The
  builder path gets sudo from ociimage's stand-in anyway (docs/images.md, "Sudo").

---

## 5. Builder design options

What wisp already has and the builder can reuse:
- **`engine.ImageCache`** (`engine/images.go`): pull, flatten and cache, one build at a time, flight joining,
  and disk-guard admission (`build` :266, `flatten` :393, `makeExt4` :422).
- **`ociimage.Inspected`** (`internal/ociimage/podman.go:53`) already reads `Config.{Env, User, WorkingDir, Entrypoint, Cmd}`.
- **`engine.Create` with `CreateSpec.ImageDisk`**: any disk becomes a sprite via `cloneFile`
  (`cp --reflink=auto --sparse=always`, `engine/lifecycle_create.go`).
- **The checkpoint path** (`engine/lifecycle_checkpoints.go:135–190`): `/internal/presuspend` (guest sync),
  then VM pause, then a reflink clone of the live disk, which gives a consistent snapshot in milliseconds on
  XFS/btrfs.
- **wisp-agent exec** (`frontend/modal/exec.go:59`, streamed stdout/stderr frames and the exit code) and the
  **fs API** (`internal/agent/fs.go:40–50`: write, mkdir, chmod, chown, read, list).
- **Egress policies** per sprite (`engine/egress.go`) and resource limits and admission.
- **Any rootfs boots.** wisp-agent is PID 1 from the initramfs; distroless already works (docs/images.md).

### (a) Host podman/buildah build, then flatten
Write the layer's Dockerfile (`FROM localhost/modal-<base-id>` substituted for `FROM base`), materialise the
context, run `podman build --build-arg … --secret …`, tag `localhost/modal-<id>`, then reuse
`ImageCache.flatten` + `makeExt4`.
- **Fidelity: best.** Real Dockerfile semantics: multi-stage, `COPY --from`, heredocs, `RUN --mount`, ARG scoping.
- **Speed.** RUN runs natively, but **every Modal layer is a full flatten**: export tar, rewrite, then mkfs.
  That is seconds to tens of seconds and a *full copy* per layer, with no block sharing between a parent's
  ext4 and its child's. Disk use is high: base 600 MB plus about 600 MB for every chained `pip_install`.
  podman's own storage holds the layers a second time, outside the disk guard.
- **Security: bad for untrusted friends.**
  - RUN executes on the **host kernel** in a rootless container, as **the daemon user who owns every VM disk,
    the token and the data directory**. A container escape compromises all of wisp.
  - Rootless networking (pasta/slirp) reaches the LAN and the host's non-loopback services, and **bypasses
    wisp's egress policy**.
  - CPU, memory and pids limits need cgroup delegation that rootless setups often lack.
  - Build-time secrets sit on the host.
- **Effort: M.** It is the smallest code path, but it hands the security problem to the operator.

### (b) Native builder VM per Modal layer (layer = reflink clone)
For each layer to build:
1. **Pick the parent disk.**
   - `FROM base`: the parent layer's disk from the Modal image store.
   - `FROM <ref>`: `ImageCache.Acquire(ref, pull=true)`, with per-pull credentials for private registries.
   - `FROM scratch`: an empty ext4.
2. **Create an internal builder sprite** (`CreateSpec.ImageDisk = parent`; it has its own quota, not
   `MaxSandboxes`), with an egress policy and CPU and memory limits. Boot it.
3. **Parse the joined commands** (`strings.Join(cmds, "\n")`) with BuildKit's Go parser (Apache-2.0):
   `frontend/dockerfile/parser`, `instructions` and `shell` (for `$VAR` expansion). This handles
   continuations, which poetry and from_dockerfile need, as well as comments, JSON vs shell form and the
   escape directive. Then execute each instruction:
   - `RUN`: agent exec as root, `SHELL`-prefixed (default `/bin/sh -c`). The env is image ENV + ARG/build_args
     + secrets' env, the workdir is the current WORKDIR. stdout and stderr go to TaskLogs.
   - `COPY <ctx-src> <dst>`: resolve against the context (§3.3) from the host content-addressed file store,
     then write it through the agent fs API with mode and chown (fallback: upload a tar and let the agent
     extract it).
   - `COPY --from=<image> <src> <dst>`: host-side `podman pull` + `podman create` + `podman cp ctr:/src -`
     (no code runs), cached by digest and path, then the fs API.
   - `ENV`, `WORKDIR` (also `mkdir -p`), `ARG`, `SHELL`, `CMD`, `ENTRYPOINT`, `USER`, `LABEL`, `EXPOSE`:
     image metadata (JSON beside the disk), inherited by children.
     `FROM <ref>` seeds that metadata from `Inspected.Config`.
4. **Commit.** Presuspend, pause and reflink-clone the sprite disk into the Modal image store
   (`<vm>/.modal-images/<im-id>.ext4` plus `.json`), on the same volume so it reflinks. Then delete the sprite.
   A failure ends the build with `FAILURE` and `exception` = the failing step and its exit code.

Properties:
- **Fidelity: good for everything the client generates.** Generated recipes use only FROM, RUN, COPY
  (context or `--from=image`), ENV, WORKDIR, SHELL, CMD, ENTRYPOINT, ARG. User Dockerfiles with multi-stage
  builds need stage support (below).
- **Speed.**
  - Boot is seconds.
  - RUN is native speed in the VM (debian_slim's apt plus build-essential is about 1–2 min, once per version).
  - A commit takes milliseconds on reflink.
  - There is no flatten and no tar round trip for chained layers. A cache hit is a map lookup, and a sandbox
    from it is one more reflink clone, exactly as today.
- **Disk.** On XFS/btrfs each layer costs only the blocks it changed. Without reflink, `cloneFile` falls back
  to a sparse full copy per layer (about 0.2 s/GB, but full space): **recommend requiring reflink for builds**
  or warning loudly.
- **Security: the strongest.**
  - Untrusted RUN stays inside Firecracker under wisp's egress policy, limits and disk guard.
  - The host does only parsing and data movement: registry pull, `podman export`/`cp`, ociimage.Rewrite and
    `mkfs.ext4 -d tar`. These are already accepted for Sprites `from.image`.
  - Secrets reach only the build VM's process env.
- **Caveats.**
  - Guest boot leaves agent artefacts on the disk (`/home/sprite`, logs, `/.sprite/…`). This is harmless
    because sandboxes boot the same way.
  - `USER` semantics: run as root unless the recipe sets USER (see decisions).
  - Multi-stage `from_dockerfile`: build each stage as its own disk. For `COPY --from=<stage>`, boot the
    stage's disk and read files via agent fs or `exec tar`, then write them into the target. Doable, but defer it.
- **Effort: L overall.** The core interpreter plus executor is 4–6 days; see §6.

### (c) = (b) with (d) as a fallback: buildah inside a builder VM
Keep a long-lived builder sprite (warm-suspended when idle) with buildah and its storage. The host sends the
Dockerfile and context in, and the VM builds. The host then streams the flattened rootfs tar out (through the
agent) into `ociimage.Rewrite` + `makeExt4`.
- **Fidelity:** buildah's, so close to Docker's.
- **Isolation:** VM-grade.
- **Costs:**
  - A flatten and a full ext4 per Modal image, so no block sharing.
  - A builder disk to maintain, with nested overlay or the vfs storage driver.
  - Slower builds (tens of seconds of export/mkfs per layer).
- **Use** only for `from_dockerfile` files that (b)'s interpreter refuses: multi-stage, `RUN --mount`, heredocs.

### Recommendation
Build **(b)** and call the hybrid "host pulls/extracts, VM runs". Reuse `ImageCache` for `FROM`, the
checkpoint clone for commits, agent exec and fs for steps, and BuildKit's parser for syntax. Refuse
unsupported Dockerfile features with a clear `FAILURE` exception. Add (d) later only if real users hit
multi-stage Dockerfiles. Do not do (a) on a server that runs friends' code.

---

## 6. Suggested phasing (effort)

| Phase | Content | Effort |
|---|---|---|
| 0 | Extend `modalpb/prune.py` KEEP with the Image*, Mount*, Blob*, Secret* RPCs and messages above, then regenerate (`gen.sh`) | S |
| 1 | Modal image store: recipe index, `im-` ID hashing, metadata JSON, failure records, GC hooks, disk-guard admission. `ImageGetOrCreate` async plus `ImageJoinStreaming` (resume by `entry_id`, end by `timeout`, `include_logs_for_finished`). `ImageFromId`. Sandbox create keyed by image disk + ENV/WORKDIR/CMD | M |
| 2 | Builder VM executor: BuildKit parser; RUN/ENV/WORKDIR/ARG/SHELL/CMD/ENTRYPOINT/USER; commit by clone; log streaming. FROM via `ImageCache` (public). This unlocks debian_slim for every Python, micromamba, from_registry, pip/apt/run_commands/env/workdir/... Prewarm command for the 5 python bases | L (4–6 d) |
| 3 | Mount and blob store: content-addressed files on the sprite volume; `MountBatchedCheckExistence`, `MountPutFile`, `MountGetOrCreate`; `BlobCreate` (multipart only) plus a signed HTTP PUT/complete endpoint on sandboxd. `COPY` from context_files and mounts. `add_local_*(copy=True)`, requirements/poetry/uv_sync, `dockerfile_commands` with context | M (2–3 d) |
| 4 | `COPY --from=<image>` (uv_pip_install, uv_sync); runtime mounts (`definition.mount_ids` written into the VM before the entrypoint) | M |
| 5 | `add_python`: global python-build-standalone mounts, fetched from GitHub with **pinned sha256** and served as synthetic GLOBAL mounts | S–M |
| 6 | Secrets in builds (needs the secrets area's store): env injection, content-hashed for the cache key; private registries (static creds, ECR token exchange, GCP `_json_key`) through a temporary podman authfile | M |
| 7 | `publish`/`from_name` (`ImagePublish`, `ImageGetByTag`, NOT_FOUND), `ImageListTags`, `ImageTagRevisions`, `ImageDelete`, `ImageBuildChainGet` + logs | S–M |
| 8 | `from_dockerfile` multi-stage (stage disks, or option (d)) | L |
| — | `run_function`, GPU builds, `volumes=` in `run_commands` | out of scope / other areas |

---

## 7. Per-item notes (with cites)

- **`_from_args`** (:607–825) is the single funnel.
  - Order inside `_load`: the context mount is loaded first (:655), then the base images are checked for
    mount layers, then the environment is fetched to learn `image_builder_version`
    (`EnvironmentGetOrCreate`; wisp answers `2025.06`), then the Dockerfile function runs, then the context
    files are read **from local disk at request time** (:692–695), then the secrets and volumes, then
    `ImageGetOrCreate`.
  - `runtime`/`runtime_debug` come from local config and should be ignored.
- **Version-dependent recipes.** `builder_version > "2024.10"` drops the modal requirements files and steps
  (:2596–2630, :2194–2209). If wisp ever reported ≤ 2024.10, every image would gain
  `COPY /modal_requirements.txt …` and `RUN uv pip install --system … -r /modal_requirements.txt`, which is
  pointless for sandboxes. Keep 2025.06.
- **Trailing whitespace.** On 2025.06 the commands are `.strip()`ed (pip_install :1153 etc.), except where
  whitespace is part of a continuation (poetry, :1683–1690: `"RUN cd /tmp/poetry && \\ "`). BuildKit's
  continuation regex tolerates whitespace after `\`; **verify with buildah/BuildKit parser tests**.
- **`env=` on any builder method** becomes `_Secret.from_dict(env)`, which is `SecretGetOrCreate{ANONYMOUS_OWNED_BY_APP, env_dict}`.
  A new secret ID per run in the capture means **secret IDs must not be in the cache key**, or `env=` builds
  never cache.
- **`gpu=`** → `gpu_config{count:1, gpu_type:"T4"}` (`parse_gpu_config`). wisp has no GPUs.
- **`from_dockerfile`** (:2415–2539) is always two images: the file, then `FROM base` plus add_python. With no
  add_python, layer 2 is just `["FROM base"]`, which the builder must treat as an identity layer (clone or alias).
  - Lines are split on `\n` verbatim, so comments, blank lines, continuation fragments and the trailing `""`
    all arrive as separate items. Never interpret item by item.
  - The context mount includes only files matched by COPY/ADD patterns (`extract_copy_command_patterns`).
    `.dockerignore` is applied client-side.
- **`dockerfile_commands`** (:1895) always computes an auto context mount; with no matching COPY it is
  `None`. With `context_files` plus a COPY whose source does not exist locally, the capture shows an **empty**
  mount (`MountGetOrCreate` with no files) alongside `context_files`, so both sources must be merged.
- **`add_local_*`.**
  - `copy=False` (:557–572) returns an image **with the parent's object ID** and a deferred mount. Any later
    build step errors client-side unless `copy=True` (:587–604).
  - `copy=True` goes through `_copy_mount` (:827), which is `["FROM base", "COPY . /"]`.
  - `add_local_python_source` puts packages under `/root/<module>` (mount.py:38 `ROOT_DIR`), filtered to
    `.py` by default.
  - Captured `mode: 436` = 0664 (the umask of the local file).
- **`add_python`** (:2170–2216): it adds `COPY /python/. /usr/local`, then `RUN ln -s …python3 …python`
  (series < 13 only), then `ENV TERMINFO_DIRS=…`. The RUN runs in the *registry image*, which may lack `ln`,
  but python images all have coreutils.
- **`micromamba`** relies on the image's own `SHELL` script and on the OCI config ENV (`MAMBA_ROOT_PREFIX`,
  `ENV_NAME`), and on the image's `USER mambauser`. Running RUN as root still works, but files come out
  root-owned under `/opt/conda`. **This needs a decision** (§8).
- **`from_registry` refs** are passed verbatim (`ubuntu:24.04`). `ociimage.ParseRef` already normalises them
  and refuses transports.
- **Private registry secrets.** The client only sends `registry_auth_type` + `secret_id`. Key names are from
  Modal's docs, not the code:
  - STATIC_CREDS: `REGISTRY_USERNAME`/`REGISTRY_PASSWORD`
  - AWS: `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_REGION`
  - GCP: `SERVICE_ACCOUNT_JSON`

  `ociimage.Podman.Pull` (:91) has no credentials parameter today; add a per-pull `--authfile`.
- **`from_id`** (:1016) sets `_object_id` eagerly. `ImageFromId` is only called when the image is loaded
  (captured on `Sandbox.create` and on layering `.pip_install` on top). Layering on a from_id image sends
  `base_images[{image_id: <that id>}]`.
- **`publish`** (:3026) sends `tag` = `name:latest` (or `env/name:tag` when an environment applies) and
  `allow_public` from `experimental_options["is_public"]`. Captured: `tag:"myimg:v1"`, no `environment_name`.
  **`from_name`** (:2987) sends `ImageGetByTag{tag}` only (`environment_name` field unused) with
  `hydrate_lazily`. Unknown tags must return gRPC NOT_FOUND; the fake's `im-nosuch` went straight into
  `SandboxCreateV2`.
- **`logs`** (:3078–3104): `ImageBuildChainGet` then AppFetchLogs per step's `builder_app_id`/`builder_task_id`
  (`_logs_manager.py:688–800`). `Image._logs()` (:2967) uses `ImageJoinStreaming{include_logs_for_finished:true}`.
- **`run_function`** (:2695): `_Function._from_local(..., is_builder_function=True)` plus a `BuildFunction`
  proto. The build container must run modal's container entrypoint, fetch the function's input and report its
  output. That is the Functions product, not images. Out of scope; refuse with a clear FAILURE.
- **Sandbox default image**: `sandbox.py:82 _default_image = _Image.debian_slim()`.
- **Mount store and volumes.** `volume.py:1423–1452` also uses `MountPutFile` (volume uploads), so the
  content-addressed file store should be shared with the volumes area.

---

## 8. Decisions for the owner

1. **Builder approach.** Recommended: (b) native builder VMs with reflink layer commits. Reject (a) host
   podman for untrusted users. Keep (d) as a later fallback for complex Dockerfiles.
2. **Scope and order.** Which phases ship first (§6)? Phases 1–2 alone unlock most real-world Modal sandbox
   code (debian_slim for any Python plus pip/apt/run_commands).
3. **Storage.** Require a reflink volume (XFS/btrfs) for building, or allow full copies? Also needed: a GC
   policy for image disks and uploaded blobs (LRU? unreferenced for N days? quota per token?) and admission
   numbers for the disk guard.
4. **Build quotas.** Concurrency (ImageCache serialises one pull at a time today), CPU, RAM and disk size for
   builder VMs, the build timeout (default suggestion: 30 min → `GENERIC_STATUS_TIMEOUT`), and whether builder
   sprites count against `MaxSandboxes`.
5. **Builder network policy.** Builds need PyPI, apt and GitHub. Use open egress minus private, host and
   metadata ranges, or the same policy as sandboxes?
6. **Who may trigger pulls and builds.** Any token holder (as Sprites `from.image` today)? A registry allowlist?
7. **Secrets in the cache key.** Use the content hash (recommended, so `env=` caches), the IDs (hosted
   behaviour unknown), or exclude them? Also: do they appear in logs (mask values)?
8. **Private registry credentials versus the shared cache.** Once a private image is pulled with user A's
   credentials, should user B be able to use the cached disk by reference? Today's ImageCache is keyed by ref
   and digest and shared.
9. **RUN user and image `USER`.** Run builds as root always (simplest; micromamba still works), or honour the
   base image's `USER` like Docker? Also, which user runs sandbox commands for images with `USER`? Today it is
   root via sudo.
10. **GPU builds.** Ignore `gpu_config` (build on CPU, with a warning line in the build log) or fail?
11. **`force_build` and `ignore_cache` semantics** as proposed in §3.4: a new ID on force, no index update on
    ignore_cache.
12. **Prewarm.** Which Python bases to prebuild (all five 2025.06 series, about 3 GB, or just the server's own
    3.14)? Retire `images/modal` or import it as the seed for the 3.14 recipe?
13. **Blob endpoint.** sandboxd needs an HTTP upload route (multipart-only, HMAC-signed URLs, size cap)
    reachable at the same public URL the gRPC uses, consistent with the `--*-public-url` proxy flags.
14. **Unsupported Dockerfile features** (multi-stage, `RUN --mount`, heredocs, `ADD <url>`): refuse with a clear
    FAILURE now, or invest in (d)?
15. **`run_function`**: confirm out of scope.

---

## 9. Unverifiable without hosted Modal

- **Image IDs.** Hosted Modal's ID function: whether it is deterministic across workspaces and clients,
  whether secret IDs, mount IDs or `context_files` bytes go into it, and whether `force_build` changes the ID.
  We only know the client never compares IDs.
- **Failed builds.** Whether hosted caches failures or retries on the next request, and what it returns for
  `ImageGetOrCreate` on a recipe that is currently building elsewhere.
- **Build environment:**
  - the RUN user and default shell
  - which env vars are present (`MODAL_*`?)
  - whether base-image `ENV`, `USER` and `WORKDIR` apply to RUN
  - network policy
  - build timeouts and resource sizes
  - how GPU builds are scheduled
- **Exact log and progress format:** the `entry_id` format, the INFO-fd lines such as step banners, when
  `IMAGE_SNAPSHOT_UPLOAD` progress is sent, and the stream end cadence against `timeout=55`.
- **Global mounts.** The real contents and layout of the `python-build-standalone.*` mounts: assumed to be the
  `install_only` tarball under `/python`, inferred from `COPY /python/. /usr/local`. The `3.14t`
  free-threaded artefact naming also needs confirming.
- **Image metadata.** What hosted fills in `ImageMetadata` (`python_packages`, `libc_version_info`) and
  whether any client path for sandboxes reads it (none found).
- **Dockerfile coverage.** Supported features in hosted `from_dockerfile`/`dockerfile_commands`:
  multi-stage, `ADD` URLs, `RUN --mount=type=secret`, heredocs, `USER`, `ONBUILD`, `HEALTHCHECK`.
- **Error mapping** for registry auth failures (FAILURE with an exception vs a gRPC error), and the
  secret-key conventions for ECR/GCP beyond the docs.
- **Publishing semantics.** `ImagePublish`/`ImageGetByTag` semantics with environments, `allow_public`, and
  the error codes for missing tags.
- **Server-side limits:** MountPutFile and BlobCreate size limits, multipart part sizes hosted chooses, and
  context file limits.

---

### Reproduce the captures
```sh
# from the repo root; V is a python with modal==1.6.0 installed (uv venv && uv pip install modal==1.6.0)
T=docs/plans/modal-spike/tools; W=$(mktemp -d)
PORT=18998 HTTP_PORT=18999 CAPLOG=$W/cap.log $V $T/fakeserver.py &   # MULTIPART=1 for multipart blobs
cd $W && MODAL_SERVER_URL=http://127.0.0.1:18998 MODAL_TOKEN_ID=ak-x MODAL_TOKEN_SECRET=as-x \
  MODAL_CONFIG_PATH=$W/modal.toml CAPLOG=$W/cap.log WORK=$W $V $OLDPWD/$T/client.py
```
`client.py` builds 30 image cases (§2), a failing build, from_id, publish/from_name and a sandbox with runtime
mounts. `client2.py` covers from_id/from_name/runtime mounts at SandboxCreateV2. `client3.py` is the multipart
blob check with `use_md5` trapped.
