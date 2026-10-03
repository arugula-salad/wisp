# Leak check: list every sandbox the probes tagged (metadata probe=wisp-e2b), kill any
# still there, and exit 1 if there were any. Uses the official SDK, so it hits the same
# target as the probes. Prints sandbox IDs only.
import sys
from e2b import Sandbox, SandboxQuery

left = []
pager = Sandbox.list(query=SandboxQuery(metadata={"probe": "wisp-e2b"}))
while pager.has_next:
    left += pager.next_items()
for s in left:
    print(f"[sweep] leaked {s.sandbox_id} state={s.state} run={s.metadata.get('run')}: killing")
    Sandbox.kill(s.sandbox_id)
total = sum(len(p) for p in [Sandbox.list().next_items()])
print(f"[sweep] {len(left)} probe sandbox(es) were left; {total} sandbox(es) on the account after cleanup")
sys.exit(1 if left else 0)
