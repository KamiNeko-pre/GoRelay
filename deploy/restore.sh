#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
source_project=${1:?usage: restore.sh SOURCE_PROJECT NEW_EMPTY_PROJECT BACKUP_DIRECTORY}
project=${2:?new isolated project required}
backup=${3:?backup directory required}
[[ "$project" != "$source_project" ]] || { echo 'Refusing restoration into the source namespace' >&2; exit 1; }
[[ -s "$backup/mysql.sql" && -s "$backup/qdrant.tar.gz" ]] || { echo 'Incomplete backup' >&2; exit 1; }
dc() { docker compose --env-file "$root/deploy/.env" -f "$root/deploy/compose.yml" -p "$project" --profile indexing "$@"; }
[[ -z $(docker ps -aq --filter "label=com.docker.compose.project=$project") ]] || { echo 'Restore project already has containers; use a fresh namespace' >&2; exit 1; }
[[ -z $(docker volume ls -q --filter "label=com.docker.compose.project=$project") ]] || { echo 'Restore project already has volumes; use a fresh namespace' >&2; exit 1; }
dc up -d --wait mysql redis
# The business user has schema privileges only; this dump creates no users.
dc exec -T mysql sh -c 'MYSQL_PWD="$MYSQL_PASSWORD" mysql -ugorelay gorelay' < "$backup/mysql.sql"
dc create qdrant
qdrant=$(dc ps -a -q qdrant)
gzip -dc "$backup/qdrant.tar.gz" | docker run --rm -i --volumes-from "$qdrant" alpine:3.21 tar -C /qdrant/storage -xf -
# Never reset budgets, resurrect cancelled Runs, or silently repeat model calls.
dc run --rm --no-deps app /app/maintenance -mode audit
dc run --rm --no-deps app /app/maintenance -mode reconcile-queue -restored-offline
printf '%s\n' 'Restore prepared. Keep the application closed until audit succeeds.' 'Start with distinct public ports: docker compose ... up -d --wait'
