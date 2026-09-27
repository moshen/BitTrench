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

## Build and run

The toolchain is pinned with mise, which also fixes `CGO_ENABLED=0` and
`-tags=noboltdb`.

```sh
mise run build     # every package, for this host
mise run test      # the full suite, ~10s, no network or VPN
mise run check     # everything CI checks
mise run release   # stripped binaries for every target, into dist/<goos>-<goarch>/
mise exec -- go run ./cmd/bittrench -config /path/to/config.toml
```

`release` can cross-compile every target from one machine with no C toolchain.

Copy `config.sample.toml` to `config.toml` and fill in the `[wireguard]`
section from your provider's configuration. `wireguard.dns` is **required**:
without it nothing can be resolved inside the tunnel.

### Where the configuration lives

`-config` wins whenever it is given, and a missing file behind it is an error
rather than a reason to load a different one. Without the flag, the first of
these that exists is used:

1. `./config.toml`
2. `$XDG_CONFIG_HOME/bittrench/config.toml`, or `~/.config/bittrench/config.toml`
3. `%ProgramData%\bittrench\config.toml`, on Windows only

The XDG location works on every platform, Windows included, so there is one
answer to where a config goes wherever the daemon runs. The machine-wide Windows
location comes last so a per-user file overrides it. If none exists, the error
names every path it tried.

The state database and the log directory default to sitting **beside the file
that was chosen**, so its location also decides where the daemon keeps its
state. Set `state_db_path` and `log_dir` to put them somewhere else.

Installing the Windows service is the one case to be deliberate about: it
registers whichever path was resolved when you ran `install`, and the service
runs as LocalSystem rather than as you. Pass `-config` with the `%ProgramData%`
path for a machine-wide service instead of letting it pick up the copy in your
own profile. `install` prints the path it registered.

### Prove the tunnel works with your config

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

There is no `-name` on `install`: one machine, one daemon. Two services sharing
this binary would share its configuration, and with it one state database and one
API port. If another service on the machine already runs this executable,
`install` refuses and tells you which one and how to remove it. `uninstall` still
takes `-name`, so a stray from an older version can be cleaned up.

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
