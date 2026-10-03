# Deletes sandboxes a probe run left behind (label run=<DAYTONA_PROBE_SWEEP's
# run ids>, or every probe=wisp-daytona sandbox with no list given), through the
# Python SDK. Prints "[sweep] leaked N" for each leftover it found, then deletes it.
import os
from daytona import Daytona, ListSandboxesQuery

runs = [r for r in os.environ.get("DAYTONA_PROBE_SWEEP", "").split(",") if r]
d = Daytona()
leaked = []
for s in d.list(ListSandboxesQuery(labels={"probe": "wisp-daytona"})):
    if not runs or s.labels.get("run") in runs:
        leaked.append(s)
for s in leaked:
    print(f"[sweep] leaked {s.id} (run {s.labels.get('run')}, state {s.state})", flush=True)
    try:
        s.delete()
    except Exception as e:
        print(f"[sweep] could not delete {s.id}: {e!r}", flush=True)
print(f"[sweep] {len(leaked)} leftover(s)", flush=True)
