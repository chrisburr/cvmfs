#!/bin/sh
# Waits for cvmfs_nfs_server and mounts it at the given mount point
# (default /cvmfs, which must exist — see synthetic.conf(5)).
set -eu

PORT="${CVMFS_NFS_PORT:-11149}"
MOUNTPOINT="${1:-/cvmfs}"

if mount | grep -q " ${MOUNTPOINT} "; then
  echo "${MOUNTPOINT} is already mounted"
  exit 0
fi

for _ in $(seq 1 60); do
  if nc -z 127.0.0.1 "$PORT" 2>/dev/null; then
    exec mount_nfs -o "ro,nolocks,nosuid,vers=3,tcp,port=${PORT},mountport=${PORT}" \
      "localhost:/" "$MOUNTPOINT"
  fi
  sleep 1
done

echo "cvmfs_nfs_server did not come up on port ${PORT}" >&2
exit 1
