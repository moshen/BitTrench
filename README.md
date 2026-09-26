# BitTrench

A single binary that brings up a **user-space WireGuard tunnel** and runs a
**BitTorrent engine entirely inside it** - peer TCP, DHT UDP, UDP tracker
datagrams, HTTP(S) tracker fetches, and every DNS lookup they imply. It exposes
a **Transmission-compatible JSON-RPC endpoint** on localhost so Sonarr and
Radarr can drive it, plus a web UI.

The whole value of the project is the word *entirely*. One host UDP socket -
the WireGuard bind - is the only thing in the process that touches the host
network stack, alongside the localhost RPC/UI listener. Every path by which a
byte or a DNS query could escape is enumerated as the invariants in
`AGENTS.md`. **Check any new networking code against them.**

## Build and run

The toolchain is pinned with mise, which also fixes `CGO_ENABLED=0` and
`-tags=noboltdb` - both deliberate, see `AGENTS.md`.

```sh
mise exec -- go build ./...
mise exec -- go test ./...
mise exec -- go run ./cmd/bittrench -config /path/to/config.toml
```

Copy `config.sample.toml` to `config.toml` and fill in the `[wireguard]`
section from your provider's configuration. `wireguard.dns` is **required**:
without it nothing can be resolved inside the tunnel and every lookup would
fall back to the host resolver, which would make the tunnel decorative.

### Prove the tunnel works before anything else

```sh
mise exec -- go run ./cmd/bittrench -config config.toml dial-through
```

This fetches `https://api.ipify.org` through the tunnel - resolution included
- and prints the exit IP. If that is not your VPN's address, nothing else in
the daemon should be trusted yet.

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

### Windows service

```
bittrench -config C:\ProgramData\bittrench\config.toml install
bittrench uninstall
```

The config path must be absolute. The SCM runs services from
`C:\Windows\System32`, so a relative path would put the state database and the
log directory somewhere unexpected - `install` rejects one.

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

## Verifying there is no leak

The in-process tests cover what can be proven without a VPN: a real encrypted
tunnel between two devices over loopback, DHT bootstrap and tracker hostnames
resolved by a DNS server that exists only inside the tunnel, and an assertion
that the netstack cannot reach a host loopback listener. **CI fails if someone
reintroduces a leak on those paths.**

What the tests cannot prove needs a live run, and is not optional before
trusting this daemon with real traffic:

1. **Packet capture on the host's real interface** during a full session. The
   only traffic to a non-loopback address must be UDP to the WireGuard
   endpoint. Port 53 to anything is a failure. Port 6881 to anything is a
   failure.
2. **A DNS server that logs**, pointed at by `wireguard.dns`. Every DHT
   bootstrap host, tracker host and webseed host must appear in its log and in
   no other resolver's.
3. **A deliberately broken tunnel** (a wrong `peer_public_key`): the daemon
   must fail to reach peers rather than quietly succeeding.
4. A TorrentDyne probe, and the two unroutable-destination cases - sending to
   an IPv6 destination on a v4-only tunnel must not kill the socket for
   subsequent v4 sends.
