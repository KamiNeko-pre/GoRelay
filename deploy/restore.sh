#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
source_project=${1:?usage: restore.sh SOURCE_PROJECT NEW_EMPTY_PROJECT BACKUP_DIRECTORY}
project=${2:?new isolated project required}
backup=${3:?backup directory required}
[[ "$project" != "$source_project" ]] || { echo 'Refusing restoration into the source namespace' >&2; exit 1; }
[[ -s "$backup/mysql.sql" && -s "$backup/snapshots.tar.gz" ]] || { echo 'Incomplete backup' >&2; exit 1; }
dc() { docker compose --env-file "$root/deploy/.env" -f "$root/deploy/compose.yml" -p "$project" "$@"; }
[[ -z $(docker ps -aq --filter "label=com.docker.compose.project=$project") ]] || { echo 'Restore project already has containers; use a fresh namespace' >&2; exit 1; }
[[ -z $(docker volume ls -q --filter "label=com.docker.compose.project=$project") ]] || { echo 'Restore project already has volumes; use a fresh namespace' >&2; exit 1; }
dc up -d --wait mysql redis
dc exec -T mysql sh -c 'MYSQL_PWD="$MYSQL_PASSWORD" mysql -ugorelay gorelay' < "$backup/mysql.sql"
dc create app
app_cid=$(dc ps -a -q app)
gzip -dc "$backup/snapshots.tar.gz" | docker run --rm -i --volumes-from "$app_cid" alpine:3.21 tar -C /app/data/snapshots -xf -
echo "Restore complete. Start with isolated ports: GORELAY_HTTP_PORT=5188 docker compose --env-file deploy/.env -f deploy/compose.yml -p $project up -d --wait"
