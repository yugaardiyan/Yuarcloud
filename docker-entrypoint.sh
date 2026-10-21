#!/bin/sh
set -e

# Make the mounted volumes writable, then drop privileges.
#
# The image runs as uid 1000, which is right — but a bind mount takes its
# ownership from the host, so a freshly created /DATA/AppData/yuarcloud/srv is
# root-owned and the server dies on its first write. The symptom is the reason
# this script exists: "permission denied" on an upload looks exactly like a full
# disk or a broken volume, and it is neither.
#
# Doing it here rather than in a README means nobody has to remember. On a
# ZimaOS box there may be no way to remember: the install is done from a web UI
# that cannot chown, so requiring it made the dashboard route impossible.
#
# Two deliberate limits:
#
#   - Not recursive. On a restart the volume holds every file the user has ever
#     uploaded, and walking it to fix ownership is minutes of I/O for no change:
#     files the server wrote are already correct, because they were created
#     after the directory was fixed. Only the top level needs setting, and
#     anything the server writes later inherits from it.
#   - Failure is a warning, not an error. A bind on NFS with root_squash, or a
#     read-only mount, will refuse the chown. Refusing to start would turn a
#     permissions warning into an outage; starting as root and carrying on is
#     worse but recoverable, and the message says what happened.

# A fresh bind mount on /config is empty, and an empty one hides the copy
# baked into the image. Without this the container started with no config and
# exited on the missing jwt secret, which read as a broken image rather than
# as an uninitialised volume.
if [ ! -f /config/config.yaml ] && [ -f /app/config.docker.yaml ]; then
  if cp /app/config.docker.yaml /config/config.yaml 2>/dev/null; then
    echo "yuarcloud: created /config/config.yaml from the image default" >&2
  else
    echo "yuarcloud: could not write /config/config.yaml" >&2
  fi
fi

if [ "$(id -u)" = "0" ]; then
  for dir in /srv /config; do
    if chown 1000:1000 "$dir" 2>/dev/null; then
      :
    else
      echo "yuarcloud: could not chown $dir" >&2
      echo "yuarcloud: if uploads fail with 'permission denied', make that" >&2
      echo "yuarcloud: directory writable by uid 1000 on the host." >&2
    fi
  done
  # The file itself, not just the directory: chown is not recursive.
  chown 1000:1000 /config/config.yaml 2>/dev/null || true
  exec su-exec yuarcloud "$@"
fi

exec "$@"