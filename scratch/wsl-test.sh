#!/bin/bash
# Re-run the suite N times under the race detector, from a fresh copy of the
# module inside the Linux filesystem. Run from Windows as:
#   wsl -d Debian -u root -- bash /mnt/c/Users/hrkcz001/zomboid-akash/scratch/wsl-test.sh [runs] [extra go test flags...]
set -u

SRC=/mnt/c/Users/hrkcz001/zomboid-akash/pzctl
DST=/root/pzctl
GO=/usr/local/go/bin/go
RUNS=${1:-1}
shift || true

rm -rf "$DST"
cp -a "$SRC" "$DST"
cd "$DST" || exit 1

if ! $GO vet ./... ; then
  echo "VET FAILED"
  exit 1
fi

status=0
for i in $(seq 1 "$RUNS"); do
  log=/root/run$i.log
  $GO test ./... -count=1 -race "$@" > "$log" 2>&1
  code=$?
  races=$(grep -c 'DATA RACE' "$log")
  fails=$(grep -c '^--- FAIL' "$log")
  echo "run $i: exit=$code races=$races failed_tests=$fails log=$log"
  if [ "$code" -ne 0 ]; then
    status=1
    grep -n '^--- FAIL' "$log" | head -20
  fi
done
exit $status
