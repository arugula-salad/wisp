#!/usr/bin/env bash
# Exercises scripts/install-service.sh without installing anything: a throwaway
# HOME, a copy of the script in a fake checkout with stub wispd/sandboxd
# binaries, and stub systemctl/loginctl/journalctl first on PATH. Nothing outside
# the temporary directory is written, and no real service is touched.
#
#   ./scripts/test-install-service.sh               # or: make test-scripts
#   BASE_REF=v1.2 ./scripts/test-install-service.sh # compare the default install to another ref
#
# The main install (name wisp, wispd, the default data directory) must render
# byte-for-byte as BASE_REF's script renders it (default origin/main; skipped if
# that ref has no install-service.sh), so production can always be reinstalled
# identically.
set -euo pipefail
cd "$(dirname "$0")/.."
SRC=$PWD
BASE_REF=${BASE_REF:-origin/main}
T=$(mktemp -d "${TMPDIR:-/tmp}/wisp-install-test.XXXXXX")
trap 'rm -rf "$T"' EXIT
H=$T/home
PROD=$H/.local/share/wisp
fails=0
pass() { echo "ok   $*"; }
fail() { echo "FAIL $*"; fails=$((fails + 1)); }

# Stubs. systemctl answers "yes, active" to everything, which also skips the
# "a daemon is already running here" probe.
mkdir -p "$T/stub" "$T/sys"
cat > "$T/stub/systemctl" <<EOF
#!/bin/sh
case "\$HOME" in "$T"/*) ;; *) echo "stub systemctl: HOME is not the test's" >&2; exit 99 ;; esac
echo "systemctl \$*" >> "$T/calls"
case " \$* " in *" show "*) echo 5s ;; esac
exit 0
EOF
printf '#!/bin/sh\necho yes\n' > "$T/stub/loginctl"
printf '#!/bin/sh\nexit 0\n' > "$T/stub/journalctl"
# Every data dir is on / unless a test says otherwise, whatever the host's /tmp is.
printf '#!/bin/sh\necho "${FAKE_DATA_MNT:-/}"\n' > "$T/stub/findmnt"
chmod +x "$T/stub/"*

# A fake checkout: the script under test plus stub daemons.
mkrepo() { # dir script-source
  mkdir -p "$1/scripts" "$1/bin"
  cp "$2" "$1/scripts/install-service.sh"
  for b in wispd sandboxd; do
    printf '#!/bin/sh\n# stub %s\n[ "$1" = status ] && echo "{\\"daemon\\": {"\nexit 0\n' "$b" > "$1/bin/$b"
    chmod +x "$1/bin/$b"
  done
}
mkrepo "$T/repo" "$SRC/scripts/install-service.sh"

mkdata() {
  mkdir -p "$1/bin" "$1/kernel" "$1/images"
  touch "$1/bin/firecracker" "$1/kernel/vmlinux" "$1/images/base.ext4" "$1/initrd.cpio"
}

# run REPO [VAR=value...] -- script args...: the script with HOME and PATH swapped out.
run() {
  local repo=$1; shift
  local envs=()
  while [ "$1" != -- ]; do envs+=("$1"); shift; done
  shift
  env -u XDG_CONFIG_HOME -u XDG_DATA_HOME -u WISP_DATA -u WISP_SYSTEM_UNIT_DIR \
    HOME="$H" USER=wisp-test PATH="$T/stub:$PATH" "${envs[@]}" \
    bash "$repo/scripts/install-service.sh" "$@"
}

fresh() { rm -rf "$H" "$T/calls"; mkdir -p "$H"; mkdata "$PROD"; }
snapshot() { (cd "$H" && find . -print0 | sort -z | xargs -0 -r stat -c '%n %s %Y %a' && find . -type f -print0 | sort -z | xargs -0 -r sha256sum); }

# ---- the main install renders exactly as the base ref's script did ----
if old=$(git show "$BASE_REF:scripts/install-service.sh" 2>/dev/null); then
  printf '%s\n' "$old" > "$T/old.sh"
  mkrepo "$T/oldrepo" "$T/old.sh"
  check_same() { # label, args...
    local label=$1; shift
    fresh; run "$T/oldrepo" -- "$@" > /dev/null; rm -rf "$T/render-old"; cp -a "$H" "$T/render-old"
    fresh; run "$T/repo" -- "$@" > /dev/null
    if diff -r "$T/render-old" "$H" > "$T/diff"; then pass "$label renders as $BASE_REF"; else fail "$label differs from $BASE_REF:"; cat "$T/diff"; fi
    # Reinstalling over what the old script wrote must work and change nothing.
    run "$T/repo" -- "$@" > /dev/null || fail "$label: reinstall over $BASE_REF's install refused"
    diff -r "$T/render-old" "$H" > /dev/null && pass "$label reinstalls identically" || fail "$label: reinstall changed files"
  }
  check_same "default install"
  check_same "default install with flags" -- --net-pool 1 --max-running 8 --confine=strict
  check_same "explicit --name wisp --data <default>" --name wisp --data "$PROD"
  # (--bin is new, so it is compared with what that ref rendered for no flags.)
  fresh; run "$T/oldrepo" -- > /dev/null; rm -rf "$T/render-old"; cp -a "$H" "$T/render-old"
  fresh; run "$T/repo" -- --bin wispd > /dev/null
  diff -r "$T/render-old" "$H" > /dev/null && pass "explicit --bin wispd renders as $BASE_REF's default" || fail "--bin wispd differs from $BASE_REF's default"
else
  echo "skip $BASE_REF has no scripts/install-service.sh"
fi

# ---- a sandboxd install beside it ----
fresh
run "$T/repo" WISP_SYSTEM_UNIT_DIR="$T/sys" -- > /dev/null
# The main install's own files: its unit, env file, lib dir and data dir.
prod_files() { (cd "$H" && find .config/systemd/user/wisp.service .config/wisp/wisp.env .local/lib/wisp/wisp .local/share/wisp -print0 | sort -z | xargs -0 stat -c '%n %s %Y %a %i' && find .config/systemd/user/wisp.service .config/wisp/wisp.env .local/lib/wisp/wisp .local/share/wisp -type f -print0 | sort -z | xargs -0 sha256sum); }
prod_before=$(prod_files)
SB=$H/.local/share/sandboxd
mkdata "$SB"
printf '[Service]\nExecStart=/bin/true\n' > "$T/sys/wisp-net2.service"
rm -f "$T/calls"
if run "$T/repo" WISP_SYSTEM_UNIT_DIR="$T/sys" -- --bin sandboxd --name sandboxd --data "$SB" -- --listen 127.0.0.1:7790 --net-pool 2 > "$T/out"; then
  LIB=$H/.local/lib/wisp/sandboxd
  want_unit="# Generated by wisp scripts/install-service.sh; re-run it rather than editing.
[Unit]
Description=wisp API daemon (sandboxd, data in $SB)
After=wisp.service

[Service]
Type=exec
EnvironmentFile=$H/.config/wisp/sandboxd.env
ExecStartPre=$LIB/wait-host.sh
ExecStart=$LIB/sandboxd --data $SB \$SANDBOXD_FLAGS"
  got_unit=$(head -n 10 "$H/.config/systemd/user/sandboxd.service")
  [ "$got_unit" = "$want_unit" ] && pass "sandboxd unit" || { fail "sandboxd unit:"; diff <(echo "$want_unit") <(echo "$got_unit"); }
  want_env="# sandboxd flags for sandboxd.service; edit, then: systemctl --user restart sandboxd
SANDBOXD_FLAGS=--listen 127.0.0.1:7790 --net-pool 2"
  [ "$(cat "$H/.config/wisp/sandboxd.env")" = "$want_env" ] && pass "sandboxd env file" || fail "sandboxd env file: $(cat "$H/.config/wisp/sandboxd.env")"
  grep -q '^  \[ -e /sys/class/net/msbr2 \] && exit 0$' "$LIB/wait-host.sh" && pass "sandboxd waits for its own bridge (msbr2)" || { fail "wait-host.sh:"; cat "$LIB/wait-host.sh"; }
  cmp -s "$LIB/sandboxd" "$T/repo/bin/sandboxd" && cmp -s "$LIB/wispd" "$T/repo/bin/wispd" && pass "sandboxd (and the wispd CLI) installed into its own lib dir" || fail "binaries in $LIB"
  [ "$(prod_files)" = "$prod_before" ] && pass "the main install's files are untouched" || fail "the sandboxd install changed the main install's files"
  grep -q 'systemctl --user restart sandboxd.service' "$T/calls" && ! grep -q ' wisp.service' "$T/calls" && pass "only sandboxd.service was (re)started" || { fail "systemctl calls:"; cat "$T/calls"; }
else
  fail "sandboxd install refused:"; cat "$T/out"
fi
# Without a wisp.service there is nothing to order after.
fresh; SB=$H/sb; mkdata "$SB"
run "$T/repo" -- --bin sandboxd --name sb --data "$SB" -- --listen 127.0.0.1:7790 --net=false > /dev/null
grep -q '^After=' "$H/.config/systemd/user/sb.service" && fail "After= without a wisp.service" || pass "no After= when wisp.service is absent"
# A data dir on a filesystem of its own (geek's /bulk is nofail) waits for that mount.
grep -q mountpoint "$H/.local/lib/wisp/sb/wait-host.sh" && fail "wait-host.sh waits for a mount with data on /" || pass "no mount wait with data on /"
rm -f "$H/.config/systemd/user/sb.service"
run "$T/repo" FAKE_DATA_MNT=/bulk -- --bin sandboxd --name sb --data "$SB" > /dev/null
grep -q "^  mountpoint -q '/bulk' && exit 0\$" "$H/.local/lib/wisp/sb/wait-host.sh" && pass "sandboxd waits for its data dir's own mount (/bulk)" || { fail "wait-host.sh with data on /bulk:"; cat "$H/.local/lib/wisp/sb/wait-host.sh"; }
# A reinstall of the same thing is fine and keeps the remembered flags.
run "$T/repo" -- --bin sandboxd --name sb --data "$SB" > /dev/null && pass "sandboxd reinstall without flags keeps its env file" || fail "sandboxd reinstall refused"

# ---- every refusal refuses and writes nothing ----
refuses() { # label, expected message, [VAR=value...] -- args...
  local label=$1 msg=$2; shift 2
  local before; before=$(snapshot)
  if run "$T/repo" "$@" > "$T/out" 2>&1; then
    fail "$label: was not refused"; return
  fi
  grep -q -- "$msg" "$T/out" || { fail "$label: unexpected message:"; cat "$T/out"; return; }
  [ "$(snapshot)" = "$before" ] && pass "$label: refused, nothing written" || fail "$label: refused but wrote files"
}
fresh
OTHER=$H/other; mkdata "$OTHER"
refuses "sandboxd without --name" "needs an explicit --name" -- --bin sandboxd --data "$OTHER" -- --listen :1
refuses "sandboxd named wisp" "needs an explicit --name" -- --bin sandboxd --name wisp --data "$OTHER" -- --listen :1
refuses "sandboxd without --data" "needs an explicit --data" -- --bin sandboxd --name sb -- --listen :1
refuses "sandboxd with only WISP_DATA" "needs an explicit --data" WISP_DATA="$OTHER" -- --bin sandboxd --name sb -- --listen :1
refuses "sandboxd on wisp's data dir" "needs an explicit --data" -- --bin sandboxd --name sb --data "$PROD" -- --listen :1
refuses "sandboxd on wisp's data dir, other spelling" "needs an explicit --data" -- --bin sandboxd --name sb --data "$PROD/../wisp/" -- --listen :1
refuses "sandboxd without --listen" "needs an explicit --listen" -- --bin sandboxd --name sb --data "$OTHER" -- --net=false
refuses "sandboxd even with --force-pair on wisp's data" "needs an explicit --data" -- --bin sandboxd --name sb --data "$PROD" --force-pair -- --listen :1
refuses "unknown --bin" "must be wispd or sandboxd" -- --bin wisp-netd --name x --data "$OTHER"
refuses "another name on wisp's data dir" "on wisp's data directory" -- --name wisp2
refuses "the name wisp on another data dir" "--name wisp is the main install" -- --data "$OTHER"
refuses "the name wisp via WISP_DATA elsewhere" "--name wisp is the main install" WISP_DATA="$OTHER" --
refuses "--check on wisp's data dir" "needs an explicit --data" -- --check --bin sandboxd --name sb --data "$PROD" -- --listen :1
before=$(snapshot)
run "$T/repo" -- --check --bin sandboxd --name sb --data "$OTHER" -- --listen :1 > /dev/null && [ "$(snapshot)" = "$before" ] \
  && pass "--check passes a good sandboxd install and writes nothing" || fail "--check on a good sandboxd install"
# --force-pair lets the two pairings through.
run "$T/repo" -- --name wisp2 --force-pair > /dev/null && pass "--force-pair: another name on wisp's data dir" || fail "--force-pair refused another name on wisp's data dir"
# Existing units are never repointed.
fresh; mkdata "$OTHER"
run "$T/repo" -- > /dev/null
refuses "repointing wisp.service at another data dir" "exists and runs" -- --data "$OTHER" --force-pair
run "$T/repo" -- --bin sandboxd --name sb --data "$OTHER" -- --listen :1 > /dev/null
OTHER2=$H/other2; mkdata "$OTHER2"
refuses "repointing sb.service at another data dir" "exists and runs" -- --bin sandboxd --name sb --data "$OTHER2" -- --listen :1
refuses "repointing sb.service at wispd" "exists and runs" -- --name sb --data "$OTHER"
sed -i 's|^ExecStart=[^ ]*|ExecStart=/opt/elsewhere/wispd|' "$H/.config/systemd/user/wisp.service"
refuses "default install over a wisp.service running another binary" "exists and runs" --

# ---- review fixes (PR #52) ----
# Uninstall with an empty or path-like name must not remove every stack's lib directory.
fresh; run "$T/repo" -- > /dev/null
refuses "uninstall with an empty --name" "bad --name" -- --uninstall --name ''
refuses "uninstall with --name .." "bad --name" -- --uninstall --name ..
refuses "uninstall with --name a/b" "bad --name" -- --uninstall --name a/b
# An env file left by the other binary's install is not silently reused.
fresh; mkdata "$OTHER"
run "$T/repo" -- --bin sandboxd --name sb --data "$OTHER" -- --listen :1 > /dev/null
run "$T/repo" -- --uninstall --name sb > /dev/null
refuses "reinstalling sb as wispd over sandboxd's env file" "has no WISPD_FLAGS= line" -- --name sb --data "$OTHER" --force-pair
run "$T/repo" -- --name sb --data "$OTHER" --force-pair -- --listen 127.0.0.1:1 > /dev/null \
  && grep -q '^WISPD_FLAGS=--listen 127.0.0.1:1$' "$H/.config/wisp/sb.env" && pass "flags after -- rewrite a stale env file" || fail "flags after -- did not rewrite the stale env file"

# ---- the Makefile's guards (make -n: nothing is run) ----
mk() { env -u NAME -u DATA -u BIN -u FLAGS -u WISP_DATA -u XDG_DATA_HOME HOME="$H" make -s -n -C "$SRC" "$@" 2>&1; }
mk install-sandboxd NAME=sb > "$T/out" && fail "make install-sandboxd without DATA" || { grep -q usage "$T/out" && pass "make install-sandboxd needs DATA" || fail "make install-sandboxd needs DATA: no usage message"; }
mk install-sandboxd DATA=/x > "$T/out" && fail "make install-sandboxd without NAME" || { grep -q usage "$T/out" && pass "make install-sandboxd needs NAME" || fail "make install-sandboxd needs NAME: no usage message"; }
mk install-service NAME=sb BIN=sandboxd DATA=/x > "$T/out" && fail "make install-service took NAME/BIN/DATA" || pass "make install-service refuses NAME/BIN/DATA"
out=$(mk install-sandboxd NAME=sb DATA=/x FLAGS='--listen :1')
check_line=$(grep -n 'install-service.sh --check' <<<"$out" | cut -d: -f1)
initrd_line=$(grep -n 'WISP_DATA=/x ./scripts/build-initrd.sh' <<<"$out" | cut -d: -f1)
[ -n "$check_line" ] && [ -n "$initrd_line" ] && [ "$check_line" -lt "$initrd_line" ] && grep -q -- '--bin sandboxd --name sb --data /x -- --listen :1$' <<<"$out" \
  && pass "make install-sandboxd checks before building the initrd into DATA" || { fail "make install-sandboxd recipe:"; echo "$out"; }
mk install-service | grep -qx './scripts/install-service.sh' && pass "make install-service is unchanged for the main install" || fail "make install-service recipe"
NAME=somehost DATA=/data BIN=/usr/bin HOME="$H" make -s -n -C "$SRC" install-service 2>&1 | grep -qx './scripts/install-service.sh' \
  && pass "make install-service ignores NAME/DATA/BIN from the environment" || fail "make install-service tripped on environment NAME/DATA/BIN"
mk initrd | grep -q "initrd: writing $H/.local/share/wisp/initrd.cpio" && pass "make initrd names its (default) data dir" || fail "make initrd output"
WISP_DATA=/x make -s -n -C "$SRC" image | grep -q "image: writing /x/images/base.ext4" && pass "make image honours WISP_DATA and says so" || fail "make image output"

echo
[ "$fails" = 0 ] && echo "all install-service.sh checks passed" || { echo "$fails check(s) failed"; exit 1; }
