# Development

```sh
make test     # unit tests, race detector. The vmm tests boot a real microVM; they skip without /dev/kvm.
              # The ACME test runs against pebble if it is on PATH (go install github.com/letsencrypt/pebble/v2/cmd/pebble@latest).
make e2e      # official Sprites Go SDK against a running spritesd
make initrd   # rebuild the agent and sprite-env; sprites pick them up on their next *cold* boot
make netd     # build the network-policy helper that setup-host.sh installs

./scripts/test-netpolicy-netns.sh     # the real nft ruleset + helper, in a rootless podman netns
./scripts/verify-network-policy.sh    # network policy on the real host, from inside real guests
./scripts/verify-backup.sh            # backs a sprite up, deletes its whole data directory, restores it
```

e2e knobs: `SPRITES_E2E_IDLE_TIMEOUT=<the daemon's --idle-timeout>` enables the lifecycle
subtests (tasks, watch); `SPRITES_SDK_DEBUG=1` shows which connection mode the SDK used.
`SPRITES_E2E_GO_CONTROL=1` (with a daemon started `--control-for-go-sdk`) runs the tests that
drive `/control` with the Go SDK. `./scripts/probe-sdks.sh` runs the official JS and Python
SDKs against a running daemon: exec with control mode off and on, port proxying, and an exec
whose VM is restored away.
The backup suite skips itself unless the daemon under test was started with a reachable
`--backup-bucket`.

Only one spritesd per host may own the tap pool. For extra dev/test stacks:

```sh
./scripts/dev-data.sh /tmp/ms-x            # keep it short: the dir holds unix sockets (108-byte limit)
MINI_SPRITES_DATA=/tmp/ms-x ./scripts/build-initrd.sh
./bin/spritesd --data /tmp/ms-x --listen 127.0.0.1:7801 --net=false
```
