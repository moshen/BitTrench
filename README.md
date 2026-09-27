# BitTrench

<img src="internal/webui/assets/favicon.svg" alt="Shovel digging bits" width="200px">

<br/>

A single binary that brings up a **user-space WireGuard tunnel** and runs a
**BitTorrent engine entirely inside it** - peer TCP, DHT UDP, UDP tracker
datagrams, HTTP(S) tracker fetches, and every DNS lookup they imply. It exposes
a **Transmission-compatible JSON-RPC endpoint** on localhost so Sonarr and
Radarr can drive it, along with a web UI.

One host UDP socket (the WireGuard connection) is the only network connection
the host machine sees, alongside the localhost RPC/UI listener.

## Install

Download a release archive from the releases page and put `bittrench` on your
PATH. Or:

### mise

Every push to `main` that passes CI publishes an archive per platform. Install
the latest with [mise](https://mise.jdx.dev) through the `github:` backend:

```sh
mise use -g "github:moshen/BitTrench"
```

That picks the archive matching your OS and architecture, and puts the extracted
`bittrench` on your PATH. `config.sample.toml` is in the same archive, next to
the binary.

To pin it from a `mise.toml` instead:

```toml
[tools."github:moshen/BitTrench"]
version = "latest"
```

`mise ls-remote github:moshen/BitTrench` lists the published versions, and a
specific one installs with `mise use -g "github:moshen/BitTrench@<version>"`.

### docker

Every release publishes two images to the GitHub container registry, one per
linux architecture, joined under a multi-arch tag - so a plain pull resolves to
the right one:

```sh
docker pull ghcr.io/moshen/bittrench:latest             # amd64 or arm64, whichever fits
docker pull ghcr.io/moshen/bittrench:2026.9.27-1        # a specific version
docker pull ghcr.io/moshen/bittrench:2026.9.27-1-arm64  # pinned to one architecture
```

The images hold the same binaries the release archives hold. They need **no
privileges and no `/dev/net/tun`**: the WireGuard device is entirely in user
space, so there is no `--cap-add NET_ADMIN` and no `--privileged` here.

```sh
mkdir -p config downloads
cp config.sample.toml config/config.toml    # fill in [wireguard]; save_path is
                                           # the image's, see below

docker run -d --name bittrench \
  --user "$(id -u):$(id -g)" \
  -p 127.0.0.1:6800:6800 \
  -v "$PWD/config:/config" \
  -v "$PWD/downloads:/downloads" \
  ghcr.io/moshen/bittrench:latest
```

- **`/config` is the working directory**, which is where `./config.toml` - the
  first entry in the search path above - resolves. So a config mounted there
  needs no `-config` flag, and `state.db` lands beside it in the same mount,
  which therefore has to be writable.
- **Downloads go to `/downloads`**, whatever `save_path` in the mounted config
  says - the image overrides it, so the directory to mount is the same for
  everyone. A config that sets no `save_path` at all works in here.
- **Logs go to stderr only**, so `docker logs -f bittrench` is the whole of
  them; the rolling files are off. `-e BITTRENCH_LOGGING_TO_FILE=1` brings them
  back, under `/config/logs`.
- **`--user` is worth passing.** The image does not fix a uid, so without it the
  daemon runs as root and everything it writes into your mounts is root-owned.
- **6800 is always the port to publish**, for the same reason as the download
  path - see [the override table](#overriding-from-the-environment). Publishing
  to `127.0.0.1` as above keeps it off the LAN; if you publish it more widely,
  set `[api] username` and `password`, which the daemon warns about at startup.

Subcommands work as arguments to the container, so a one-shot download is:

```sh
docker run --rm -v "$PWD/config:/config" -v "$PWD/downloads:/downloads" \
  ghcr.io/moshen/bittrench:latest get 'magnet:?xt=urn:btih:...'
```

There is no shell in the image. `config.sample.toml` ships inside it anyway, so
the version that matches the binary can be lifted out:

```sh
id=$(docker create ghcr.io/moshen/bittrench:latest)
docker cp "$id:/usr/share/bittrench/config.sample.toml" .
docker rm "$id"
```

### scoop, on Windows

[moshen/BitTrench-scoop](https://github.com/moshen/BitTrench-scoop) is a
[Scoop](https://scoop.sh) bucket holding a manifest for the windows-amd64
archive:

```powershell
scoop bucket add bittrench https://github.com/moshen/BitTrench-scoop
scoop install bittrench
```

Then `scoop update bittrench` for later versions.

Put your `config.toml` under `%USERPROFILE%\.config\bittrench\` or
`C:\ProgramData\bittrench\` for a scoop install. `scoop prefix bittrench` prints
the install directory, if you want the `config.sample.toml` that shipped in the
archive.

## Configuration

See [config.sample.toml](config.sample.toml) for a full configuration
reference.

The `-config` command line flag will prevent searching for a user config file.
Without the flag, the first of these that is found will be used:

1. `./config.toml`
2. `$XDG_CONFIG_HOME/bittrench/config.toml`, or `~/.config/bittrench/config.toml`
3. `%ProgramData%\bittrench\config.toml`, on Windows only

The XDG location works on every platform, Windows included.

Installing the Windows service registers whichever path was resolved when you
run `install`, and the service runs as `LocalSystem` rather than as you. Pass
`-config` with the `%ProgramData%` path for a machine-wide service instead of
letting it pick up the copy in your own profile. Or, create a configuration in
`%ProgramData%` before installing the service. `install` prints the path it
registered.

The state database and the log directory default to sitting **beside the cofig
file that was found**. Set `state_db_path` and `log_dir` to put them somewhere
else.

### Overriding from the environment

Four keys, and only these four, can also come from the environment. Each
variable is named after the key it overrides:

| Variable | Overrides | The image sets |
|---|---|---|
| `BITTRENCH_API_LISTEN_INTERFACE` | `[api] listen_interface` | `0.0.0.0` |
| `BITTRENCH_API_LISTEN_PORT` | `[api] listen_port` | `6800` |
| `BITTRENCH_TORRENT_SAVE_PATH` | `[torrent] save_path` | `/downloads` |
| `BITTRENCH_LOGGING_TO_FILE` | `[logging] to_file` | `0` |

They exist for the container image, where the config file is yours but the wiring
is the image's - so a config written for a host, or copied between deployments,
cannot change how the container is reached, where it writes, or where its logs
go. A published port has to reach a listener on a routable interface, and
`127.0.0.1` inside a container is reachable from nothing outside it; the writable
mount is the image's to name; and a container's logs belong on stderr, where
`docker logs` already collects them.

Unset or empty leaves the file's value alone, and a value that is not an IP
address, a port or a boolean is a startup error naming the variable. Everything
else comes from the file. `BITTRENCH_LOG` sets the log level (`debug`, `info`,
`warn`, `error`) and is not part of the schema.

## Build and run

The toolchain is pinned with mise, which also fixes `CGO_ENABLED=0` and
`-tags=noboltdb`.

```sh
mise run build     # every package, for this host
mise run test      # the full suite, ~10s, no network or VPN
mise run check     # everything CI checks
mise run release   # stripped binaries for every target, into dist/<goos>-<goarch>/
mise run docker    # the container images, from what release built
mise exec -- go run ./cmd/bittrench -config /path/to/config.toml
```

`release` can cross-compile every target from one machine with no C toolchain.

`mise run docker` packages the `dist/linux-*` trees `release` just built into
container images - this host's architecture by default, both with `PUSH=1`,
which is what the release workflow runs. It compiles nothing itself, so the
image and the archive for a version carry the same bytes.

Copy `config.sample.toml` to `config.toml` and fill in the `[wireguard]`
section from your provider's configuration. `wireguard.dns` is **required**:
without it nothing can be resolved inside the tunnel.

### Test the tunnel works with your config

```sh
mise exec -- go run ./cmd/bittrench -config config.toml dial-through
```

This fetches `https://api.ipify.org` through the tunnel and prints the exit IP.
If that is not your VPN's address, nothing else in the daemon should be trusted.

### Download one torrent and exit

```sh
bittrench -config config.toml get ./debian.torrent
bittrench -config config.toml get -dir /mnt/media -timeout 2h 'magnet:?xt=urn:btih:...'
```

`get` takes one torrent - a magnet URI, an http(s) URL, or a path to a local
`.torrent` - brings up the tunnel, downloads it, and exits. It opens **no host
listener**: the Transmission RPC endpoint and the web UI are forced off for the
run, so it works while the daemon proper is running and holding that port. It
also does not restore the other torrents in the state database, so a one-shot
run never starts seeding everything the daemon knows about.

The torrent is still recorded in `state.db`, so an interrupted `get` resumes
where it left off, and the daemon picks it up on its next start. It exits 0 once
every selected file is complete, and non-zero on a torrent error or a `-timeout`
that expires. With `allowed_extensions` set, "complete" means the files that
passed the filter - the rest are never requested.

To run the daemon itself with no RPC endpoint and no web UI, set
`enabled = false` in the `[api]` table. That leaves the WireGuard bind as the
only host socket the process holds.

### The download queue

`[torrent] download_queue_size` (default 5) caps how many torrents download at
once; the rest wait their turn and report Transmission's download-wait status,
which clients show as "Queued". Finished torrents seed without holding a slot,
and a torrent stopped by hand does not hold one either. Set it to `0` to
download everything at once.

The order is persisted, so it survives a restart, and a client can reorder it
with `queue-move-top`, `queue-move-up`, `queue-move-down` and
`queue-move-bottom` - which is how Sonarr promotes a download whose priority is
set to First.

### Windows service

```
bittrench -config C:\ProgramData\bittrench\config.toml install
bittrench uninstall
```

The config path must be absolute, because the SCM runs services from
`C:\Windows\System32` and the state database and log directory are resolved
beside the config file.

Running `install` again **updates** the registration rather than refusing. It
removes and recreates the service, which means:

- The executable path is rewritten, so moving `bittrench.exe` and re-running
  `install` is how you point the service at its new location. A service is always
  registered by full path.
- The `-config` path, start type, display name and description are rewritten too.
- Anything customised by hand in `services.msc`, such as recovery actions or a
  non-default log-on account, goes back to its default. `install` warns when it
  has replaced a registration.
- A service that was running is stopped, recreated and started again. One that
  was stopped stays stopped unless you pass `-start-now`.

## Layout

| Package | Responsibility |
|---|---|
| `cmd/bittrench` | CLI, subcommands, wiring, shutdown ordering |
| `internal/config` | the TOML schema, defaults and validation |
| `internal/tunnel` | the WireGuard device and the user-space network stack |
| `internal/engine` | the torrent client, filters, limits, and all per-torrent state |
| `internal/store` | SQLite: torrents, gids, file selections, piece completion |
| `internal/rpc` | the Transmission JSON-RPC endpoint |
| `internal/api` | the native JSON API the web UI uses |
| `internal/webui` | the embedded single-page UI |
| `internal/server` | the one host listener, on localhost |
| `internal/logging` | the rolling file writer and slog setup |
| `internal/service` | the Windows SCM integration |
| `internal/tunneltest` | a real two-ended tunnel, for tests |

## Verifying there are no leaks

The unit tests cover what can be proven without a VPN: a real encrypted
tunnel between two devices over loopback, DHT bootstrap and tracker hostnames
resolved by a DNS server that exists only inside the tunnel, and asserts
that the netstack cannot reach a host loopback listener. **CI fails if a leak is
introduced on those paths.**

What the tests cannot prove needs a live run, and has been verified manually:

1. **Packet capture on the host's real interface** during a full session. The
   only traffic to a non-loopback address must be UDP to the WireGuard
   endpoint. Port 53 to anything is a failure. Any other connections are a
   failure.
2. **A DNS server that logs**, pointed at by `wireguard.dns`. Every DHT
   bootstrap host, tracker host and webseed host must appear in its log and in
   no other resolver's.
3. **A deliberately broken tunnel** (a wrong `peer_public_key`). The daemon
   must fail to reach anything.
