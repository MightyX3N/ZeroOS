# Sets Pterodactyl up after its first start, so it works right away:
# an account, this computer as the node servers run on, ports for the first
# servers, and the settings of Wings (the part that runs the servers).
# ZeroOS runs this once, with the answers from the install questions in the environment.
set -eu

dc() { docker compose -p "$ZEROOS_PROJECT" --project-directory "$ZEROOS_APP_DIR" -f "$ZEROOS_APP_DIR/docker-compose.yml" "$@"; }
artisan() { dc exec -T panel php artisan "$@"; }

SERVERS="$APP_DATA_DIR/data/wings/servers"   # same place inside Wings and on this computer
WINGS_PORT=28080
FIRST_PORT=25565
LAST_PORT=25600

echo "Waiting for Pterodactyl's database to be ready"
n=0
until artisan p:node:list >/dev/null 2>&1; do
  n=$((n + 1))
  if [ "$n" -gt 120 ]; then echo "Pterodactyl didn't get ready in time."; exit 1; fi
  sleep 3
done

echo "Making your account"
artisan p:user:make --email="$PTERO_EMAIL" --username="$PTERO_USERNAME" --name-first="$PTERO_USERNAME" \
  --name-last="Admin" --password="$PTERO_PASSWORD" --admin=1 >/dev/null

echo "Adding this computer as the place servers run on"
mem_mb=$(awk '/MemTotal/ {printf "%d", $2/1024}' /proc/meminfo)
disk_mb=$(df -Pm "$APP_DATA_DIR" | awk 'NR==2 {print $4}')
artisan p:location:make --short=home --long="At home" >/dev/null
artisan p:node:make --name="$HOST_NAME" --description="Made by ZeroOS" --locationId=1 \
  --fqdn="$HOST_NAME.local" --public=1 --scheme=http --proxy=0 --maintenance=0 \
  --maxMemory="$mem_mb" --overallocateMemory=0 --maxDisk="$disk_mb" --overallocateDisk=0 \
  --uploadSize=100 --daemonListeningPort="$WINGS_PORT" --daemonSFTPPort=2022 \
  --daemonBase="$SERVERS/volumes" >/dev/null

echo "Setting aside ports $FIRST_PORT to $LAST_PORT for your servers"
artisan tinker --execute="app(\\Pterodactyl\\Services\\Allocations\\AssignmentService::class)->handle(\\Pterodactyl\\Models\\Node::query()->firstOrFail(), ['allocation_ip' => '0.0.0.0', 'allocation_alias' => '$HOST_NAME.local', 'allocation_ports' => ['$FIRST_PORT-$LAST_PORT']]);" >/dev/null

echo "Setting up Wings, the part that runs the servers"
cfg=$(artisan p:node:configuration 1 --format=json)
field() { printf '%s\n' "$cfg" | sed -n "s/.*\"$1\": *\"\\([^\"]*\\)\".*/\\1/p" | head -n 1; }
uuid=$(field uuid); token_id=$(field token_id); token=$(field token)
if [ -z "$uuid" ] || [ -z "$token_id" ] || [ -z "$token" ]; then echo "Couldn't read the node's settings from Pterodactyl."; exit 1; fi

mkdir -p "$APP_DATA_DIR/data/wings/etc" "$SERVERS"
umask 077
cat > "$APP_DATA_DIR/data/wings/etc/config.yml" <<YML
# Written by ZeroOS when Pterodactyl was installed.
debug: false
uuid: $uuid
token_id: $token_id
token: $token
api:
  host: 0.0.0.0
  port: $WINGS_PORT
  ssl:
    enabled: false
  upload_limit: 100
system:
  root_directory: $SERVERS
  log_directory: /var/log/pterodactyl
  data: $SERVERS/volumes
  archive_directory: $SERVERS/archives
  backup_directory: $SERVERS/backups
  tmp_directory: $SERVERS/tmp
  username: pterodactyl
  timezone: $TZ
  passwd:
    directory: $SERVERS/run/etc
  machine_id:
    directory: $SERVERS/run/machine-id
  sftp:
    bind_port: 2022
docker:
  # The game servers' own network. Docker hands 172.18 to other apps, so use a range nothing else takes.
  network:
    interface: 10.213.77.1
    interfaces:
      v4:
        subnet: 10.213.77.0/24
        gateway: 10.213.77.1
allowed_mounts: []
# Wings talks to the panel inside Docker; browsers may come from any of this computer's addresses.
remote: http://panel
allowed_origins:
  - "*"
ignore_panel_config_updates: true
YML

dc restart wings >/dev/null 2>&1 || dc up -d wings >/dev/null

echo "Checking that Wings answers"
# The panel reaches Wings the same way your browser will. Two answers in a row,
# because a Wings that keeps crashing is reachable for a moment each time it restarts.
answers() { [ "$(dc exec -T panel curl -s -o /dev/null -m 5 -w '%{http_code}' "http://$HOST_NAME.local:$WINGS_PORT/api/system" 2>/dev/null)" = "401" ]; }
n=0
until answers && sleep 4 && answers; do
  n=$((n + 1))
  if [ "$n" -gt 30 ]; then
    echo "Wings didn't start. Its last lines:"
    dc logs --no-color --tail 8 wings 2>&1 | sed 's/\x1b\[[0-9;]*m//g' | cut -c1-300
    exit 1
  fi
  sleep 3
done
echo "Pterodactyl is ready. Sign in with the username and password you chose."
