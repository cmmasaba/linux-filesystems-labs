#!/usr/bin/env bash
# End-to-end check of gcs.out through a real FUSE mount.
#
#   ./gcs-backed/e2e.sh BUCKET          # real GCS (needs ADC and objectUser on BUCKET)
#   STORAGE_EMULATOR_HOST=127.0.0.1:4443 ./gcs-backed/e2e.sh lab   # local emulator
#
# Run from fuse-filesystems/. Everything happens under a fresh top-level
# directory e2e-<timestamp>/ in the bucket, which is deleted at the end.
# The remount step checks that data really reached the bucket.
set -euo pipefail
export LC_ALL=C # byte-order ls output

BUCKET=${1:?usage: e2e.sh BUCKET}
MNT=$(mktemp -d /tmp/gcsfs-e2e.XXXXXX)
WORK=$(mktemp -d /tmp/gcsfs-e2e-work.XXXXXX)
DIR="e2e-$(date +%s)"
BIN="$PWD/gcs.out"
PID=

pass=0
fail=0
ok()   { echo "  PASS $*"; pass=$((pass + 1)); }
bad()  { echo "  FAIL $*"; fail=$((fail + 1)); }
expect() { # expect DESCRIPTION ACTUAL EXPECTED
  if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1: got '$2', want '$3'"; fi
}

mount_fs() {
  "$BIN" --bucket "$BUCKET" --stat-ttl=1s "$MNT" -o attr_timeout=1,entry_timeout=1,negative_timeout=1 \
    >>"$WORK/gcs.log" 2>&1 &
  PID=$!
  for _ in $(seq 50); do mountpoint -q "$MNT" && return 0; sleep 0.1; done
  echo "mount did not come up; log:"; cat "$WORK/gcs.log"; exit 1
}

unmount_fs() {
  fusermount3 -u "$MNT"
  wait "$PID" || true
  PID=
}

# Runs on every exit, including failures: never leave a mount or a running
# gcs.out behind (a busy mount would otherwise hang the caller).
cleanup() {
  set +e
  cd /
  if [[ -n "$PID" ]]; then
    rm -rf "${MNT:?}/$DIR" 2>/dev/null
    fusermount3 -u "$MNT" 2>/dev/null || fusermount3 -uz "$MNT"
    kill "$PID" 2>/dev/null
    wait "$PID" 2>/dev/null
    if [[ $fail -gt 0 || $pass -eq 0 ]]; then echo "--- gcs.out log ---"; cat "$WORK/gcs.log"; fi
  fi
  rmdir "$MNT" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "building"
go build -tags fuse3 -o "$BIN" ./gcs-backed

echo "mounting gs://$BUCKET at $MNT (test dir $DIR)"
mount_fs
cd "$MNT"

echo "files and directories"
mkdir -p "$DIR/d/sub"
echo hello > "$DIR/d/a.txt"
expect "write + read back" "$(cat "$DIR/d/a.txt")" "hello"
expect "ls" "$(ls "$DIR/d" | tr '\n' ' ')" "a.txt sub "
echo more >> "$DIR/d/a.txt"
expect "append" "$(cat "$DIR/d/a.txt" | tr '\n' ' ')" "hello more "
truncate -s 3 "$DIR/d/a.txt"
expect "truncate" "$(cat "$DIR/d/a.txt")" "hel"

echo "10 MiB copy"
head -c $((10 << 20)) /dev/urandom > "$WORK/big"
cp "$WORK/big" "$DIR/d/big"
expect "md5 of 10 MiB file" "$(md5sum < "$DIR/d/big")" "$(md5sum < "$WORK/big")"
expect "size" "$(stat -c %s "$DIR/d/big")" "$((10 << 20))"

echo "metadata"
chmod 600 "$DIR/d/a.txt"
expect "chmod" "$(stat -c %a "$DIR/d/a.txt")" "600"
touch -m -d '2020-01-02 03:04:05' "$DIR/d/a.txt"
expect "mtime" "$(stat -c %Y "$DIR/d/a.txt")" "$(date -d '2020-01-02 03:04:05' +%s)"

echo "renames"
mv "$DIR/d/a.txt" "$DIR/d/b.txt"
expect "file rename" "$(cat "$DIR/d/b.txt")" "hel"
[[ ! -e "$DIR/d/a.txt" ]] && ok "old name gone" || bad "old name still present"
mv "$DIR/d" "$DIR/e"
expect "dir rename keeps contents" "$(ls "$DIR/e" | tr '\n' ' ')" "b.txt big sub "
expect "dir rename keeps file mode" "$(stat -c %a "$DIR/e/b.txt")" "600"

echo "removal"
if rmdir "$DIR/e" 2>/dev/null; then bad "rmdir of non-empty dir succeeded"; else ok "rmdir non-empty refused"; fi
rmdir "$DIR/e/sub" && ok "rmdir empty"
df -h "$MNT" >/dev/null && ok "df works"

echo "remount (proves the data is in the bucket, not just in this process)"
cd /
unmount_fs
mount_fs
cd "$MNT"
expect "big file after remount" "$(md5sum < "$DIR/e/big")" "$(md5sum < "$WORK/big")"
expect "content after remount" "$(cat "$DIR/e/b.txt")" "hel"
expect "mode after remount" "$(stat -c %a "$DIR/e/b.txt")" "600"
expect "mtime after remount" "$(stat -c %Y "$DIR/e/b.txt")" "$(date -d '2020-01-02 03:04:05' +%s)"

echo "cleanup"
rm -r "${DIR:?}"
[[ ! -e "$DIR" ]] && ok "rm -r" || bad "rm -r left $DIR"
cd /

echo
echo "GCS calls (from the log at unmount):"
unmount_fs
grep -o 'gcs_calls=.*' "$WORK/gcs.log" | tail -1
grep -E 'level=(WARN|ERROR)' "$WORK/gcs.log" && echo "(warnings above)" || true
echo
echo "$pass passed, $fail failed"
[[ $fail -eq 0 ]]
