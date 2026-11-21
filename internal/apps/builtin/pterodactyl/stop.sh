# Stopping Pterodactyl also stops the game servers it started.
# They are separate containers that Wings made, so "docker compose stop" leaves them running.
ids=$(docker ps -q --filter "label=Service=Pterodactyl")
[ -z "$ids" ] || docker stop $ids >/dev/null 2>&1 || true
