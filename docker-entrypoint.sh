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
  exec su-exec yuarcloud "$@"
fi

exec "$@"