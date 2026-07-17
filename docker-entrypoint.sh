#!/bin/sh
set -e

# When started as root (the default), fix ownership of the bind-mounted data
# dir, then drop privileges. Files the app creates later are owned correctly,
# so only the top level and leftovers from earlier root runs need fixing.
if [ "$(id -u)" = "0" ]; then
    dir="${SHARE_DATA_DIR:-/data}"
    mkdir -p "$dir"
    for p in "$dir" "$dir"/files "$dir"/tmp "$dir"/share.db "$dir"/share.db-wal "$dir"/share.db-shm "$dir"/secret; do
        [ -e "$p" ] && chown share:share "$p"
    done
    exec su-exec share:share /app/share
fi

exec /app/share
