# Removing Pterodactyl also removes the game server containers Wings made, and their network.
ids=$(docker ps -aq --filter "label=Service=Pterodactyl")
[ -z "$ids" ] || docker rm -f $ids >/dev/null 2>&1 || true
docker network rm pterodactyl_nw >/dev/null 2>&1 || true
