#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
project=${1:?usage: backup.sh PROJECT OUTPUT_DIRECTORY}
destination=${2:?output directory required}
mkdir -p -- "$destination"
destination=$(cd -- "$destination" && pwd)
dc() { docker compose --env-file "$root/deploy/.env" -f "$root/deploy/compose.yml" -p "$project" "$@"; }
running=$(dc ps --services --status running)
if [[ -z "$running" ]]; then echo 'No running source deployment' >&2; exit 1; fi
trap 'dc start $running >/dev/null' EXIT
dc stop -t 40 edge app publisher worker lua-runner
dc exec -T mysql sh -c 'MYSQL_PWD="$MYSQL_PASSWORD" mysqldump -ugorelay --single-transaction --no-tablespaces --hex-blob gorelay' > "$destination/mysql.sql"
app_cid=$(dc ps -a -q app)
docker run --rm --volumes-from "$app_cid:ro" alpine:3.21 tar -C /app/data/snapshots -cf - . | gzip > "$destination/snapshots.tar.gz"
dc images --format json > "$destination/images.json"
printf '%s\n' "project=$project" "created_utc=$(date -u +%FT%TZ)" 'writers_stopped=true' 'redis=derived_from_mysql' > "$destination/manifest.txt"
echo "Consistent SQL + Snapshot backup: $destination"
