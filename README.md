# ZeroOS

ZeroOS turns a mini PC running plain Debian into a friendly home server. You get a desktop in your browser where apps install with one click, keep their big files on your NAS, and can be shared online through [playit.gg](https://playit.gg) without touching your router.

It is not an operating system and not a NAS. It is one small program that runs on top of Debian and uses Docker underneath, so you never have to.

- **A desktop in the browser.** Apps are icons; they open in windows. On a phone it becomes a home screen.
- **A store that fills itself.** Apps come from the [Runtipi app store](https://github.com/runtipi/runtipi-appstore) catalog and run unchanged. No ports, paths or YAML to fill in.
- **Updates you can undo.** An app stays on its version until you update it. An update first keeps a copy of the app's own data; a new version that doesn't start is put back by itself, and any update can be undone for a week. Apps published under a moving name like `:latest` are checked for newer builds too.
- **Your own apps.** Paste a `docker-compose.yml` and it becomes an app like the ones from the Store. ZeroOS moves the page to a free port, keeps volumes in the app's folder, asks where other folders should be, and runs the rest as written.
- **Apps that need setting up are set up for you.** A few apps come with ZeroOS itself (`internal/apps/builtin`), in the same format plus a readable `setup.sh`. Pterodactyl is the first: after install it has your account, this computer as its node, ports for servers and a working Wings. Korie is there too.
- **Your NAS for big files.** Add an SMB share once, then pick folders with a folder picker. Each app's own data stays on the mini PC's SSD. If the NAS goes offline, apps that use it wait and start again by themselves.
- **Share online.** Link a playit.gg account and flip a switch per app, or share any port (for game servers).
- **Users and permissions.** Every action is a permission. Each person only sees the apps shared with them.

## Install

On Debian 13 (Debian 12, Ubuntu Server and Raspberry Pi OS 64-bit also work):

```sh
curl -fsSL https://raw.githubusercontent.com/MightyX3N/ZeroOS/main/install.sh | sudo bash
```

When it finishes it prints the web address and a setup code. Open the address on your phone or PC, type the code, and follow the setup.

Prefer to read it first?

```sh
curl -fsSL https://raw.githubusercontent.com/MightyX3N/ZeroOS/main/install.sh -o install.sh
less install.sh
sudo bash install.sh
```

### Manual install

These are the same steps the script does.

1. Install the small tools ZeroOS needs: `sudo apt install ca-certificates curl cifs-utils avahi-daemon`
2. Install Docker with the Compose plugin, following [Docker's guide for Debian](https://docs.docker.com/engine/install/debian/).
3. Download `zeroos-linux-amd64` (or `-arm64`) and `checksums.txt` from the [newest release](https://github.com/MightyX3N/ZeroOS/releases/latest), check it with `sha256sum -c`, and put it at `/usr/local/bin/zeroos` with `chmod 755`.
4. Create `/etc/systemd/system/zeroos.service` (its content is in step 5 of `install.sh`), then run `sudo systemctl enable --now zeroos`.
5. Get the address and code: `sudo zeroos web-address` and `sudo zeroos setup-code`.

## Command line

For when the web page can't be reached, or for a quick look over SSH. Run them with `sudo`.

| Command | Does |
| --- | --- |
| `zeroos status` | Web address, apps, NAS shares, playit |
| `zeroos apps` | List apps; `zeroos apps start\|stop\|restart\|logs <app>` |
| `zeroos reset-password <user>` | New password for a user, and turns off their two-step sign-in |
| `zeroos setup-code` | Show the setup code of a new install |
| `zeroos doctor` | Check Docker, NAS shares, playit and disk space, in plain words |
| `zeroos update` | Update ZeroOS to the newest release (apps keep running) |

## Where things are

| What | Where |
| --- | --- |
| ZeroOS's database and each app's own data | `/var/lib/zeroos` |
| NAS shares | mounted under `/mnt/zeroos` |
| The program | `/usr/local/bin/zeroos` |

Settings can be changed with environment variables in the service file: `ZEROOS_LISTEN` (default `:80`), `ZEROOS_DATA`, `ZEROOS_MOUNTS`.

## Building from source

```sh
go build -o zeroos ./cmd/zeroos
go test ./...
```

The web UI in `web/static` is plain HTML, CSS and JavaScript and is built into the program.

## Thanks

- App catalog: [runtipi/runtipi-appstore](https://github.com/runtipi/runtipi-appstore) (GPL-3.0)
- Korie: [MightyX3N/Korie](https://github.com/MightyX3N/Korie), run from its official image
- Pterodactyl: [panel](https://github.com/pterodactyl/panel) and [Wings](https://github.com/pterodactyl/wings) (MIT), run from their official images
- Tunnels: playit.gg's official agent, [playit-cloud/playit-agent](https://github.com/playit-cloud/playit-agent) (BSD-2-Clause)

## Licence

GPL-3.0. See [LICENSE](LICENSE).
