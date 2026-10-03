#!/usr/bin/env bash
# Regenerates this package from the pinned modal client: installs it in a
# throwaway venv, prunes its embedded protocol descriptors to the RPCs
# frontend/modal serves (prune.py), checks the result against the client's own
# descriptors (check.py), and compiles it with protoc.
#
# Needs protoc, uv and go. Bump MODAL_VERSION (and e2e/providers/modal/run.sh's)
# together; a method that prune.py keeps and the new client lacks fails here.
set -euo pipefail
MODAL_VERSION=1.6.0
PROTOC_GEN_GO=v1.36.11
PROTOC_GEN_GO_GRPC=v1.6.2

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
uv venv -q "$work/venv"
uv pip install -q -p "$work/venv/bin/python" "modal==$MODAL_VERSION"
GOBIN="$work/bin" go install "google.golang.org/protobuf/cmd/protoc-gen-go@$PROTOC_GEN_GO"
GOBIN="$work/bin" go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@$PROTOC_GEN_GO_GRPC"

cd "$here"
mkdir -p modal_proto
"$work/venv/bin/python" prune.py modal_proto
protoc -I . --include_imports -o "$work/pruned.pb" modal_proto/*.proto
"$work/venv/bin/python" check.py "$work/pruned.pb"
cp "$work"/venv/lib/python3*/site-packages/modal-"$MODAL_VERSION".dist-info/licenses/LICENSE LICENSE
PATH="$work/bin:$PATH" protoc -I . \
  --go_out=. --go_opt=module=github.com/arugula-salad/wisp/frontend/modal/modalpb \
  --go-grpc_out=. --go-grpc_opt=module=github.com/arugula-salad/wisp/frontend/modal/modalpb \
  modal_proto/api.proto modal_proto/task_command_router.proto
