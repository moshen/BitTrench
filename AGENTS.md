# BitTrench

A user-space WireGuard BitTorrent daemon. One host UDP socket carries WireGuard;
a localhost listener serves the Transmission RPC endpoint and the web UI.
Everything else - peer TCP, DHT, trackers, and every DNS lookup they imply -
runs inside the tunnel.

## Before committing

ALWAYS run `mise exec -- gofmt -l cmd internal` and fix every file it names
ALWAYS run `mise exec -- go vet ./...` and fix every finding
ALWAYS run `mise exec -- go test ./...` and have it pass
ALWAYS run `mise exec -- biome check --write` and then `biome check` after editing anything under `internal/webui/assets/`
NEVER mix a formatting-only change into a behavioural one - separate commits

## Tests

`mise exec -- go test ./...` takes about ten seconds and needs **no network, no
VPN and no privileges**. It stands up two real WireGuard devices over loopback
with an in-tunnel DNS server, so the leak paths are covered by real traffic
rather than by mocks. A failure there is never "flaky infrastructure"; read it.

- `internal/tunneltest` builds the two-ended tunnel. Use it for anything that
  needs a network, rather than reaching for the host stack in a test.
- `cmd/bittrench` has tests for the argument handling only. The one-shot
  `get` path shares its bring-up and its shutdown with the daemon through
  `openSession`/`session.close`, so a change to either is exercised by both.
- `internal/e2e` drives a real `.torrent` through the whole stack. It **skips**
  unless `BITTRENCH_E2E_TORRENT` points at one, so a green suite does not mean
  it ran. Run it by hand after touching the engine, the RPC layer or the API.
- Prefer table-driven subtests with `t.Run`, and `t.TempDir()` over anything
  that writes outside the test's own directory.

## The toolchain is pinned, deliberately

`mise.toml` fixes `CGO_ENABLED=0` and `GOFLAGS=-tags=noboltdb`. Cgo-free keeps
the SQLite driver pure Go, so a Windows cross-build needs no toolchain; the tag
keeps `go.etcd.io/bbolt` - a second, unused piece-completion backend that the
storage package would otherwise select - out of the binary. Always invoke Go
through `mise exec --` so both apply. Do not override either without a reason
you can write down.

Verify after any dependency change:

```sh
mise exec -- go list -deps ./... | grep -Ei "bbolt|llsqlite|zombiezen"   # expect nothing
mise exec -- env GOOS=windows go vet ./...                              # the service code is build-tagged
```

## Go in this codebase

- ALWAYS practice YAGNI and DRY; prefer the standard library over a dependency.
  Four third-party modules are load-bearing and were argued for; a fifth needs
  the same argument.
- Match the surrounding code - its naming, its comment density, its idiom.
- Wrap errors with `%w` and a sentence naming what failed, in the voice of the
  operator reading the log: `failed to open the state database %s: %w`.
- Interfaces are declared by the consumer, not the implementer. `engine.Net` and
  `rpc.Torrents` exist so those packages state exactly what they need - and in
  `engine.Net`'s case, so the complete list of ways the package can reach a
  network is one readable block.
- Every exported identifier has a doc comment beginning with its own name.
- Comments explain *why*. The what is already in the code; the why is usually a
  trap in a dependency, and is the reason the comment pays for itself.
- Guard shared state with the owning struct's mutex and keep the critical
  section small. Never return a live internal slice or map - copy it.
- `context.Context` is the first parameter and cancellation is honoured;
  background goroutines exit on it and are waited on during shutdown.
- No `panic` outside `init`. A daemon that dies on bad input is worse than one
  that reports it.

## Invariants - not style preferences

Breaking any of these is a bug even when the tests pass.

1. **Every socket goes through `internal/tunnel`.** Use its `DialContext`,
   `ListenUDP`, `ListenTCP` and `HTTPClient` helpers, never `netstack.Net`
   directly and never the host stack. `netstack`'s own `ListenUDP` with a
   portless `UDPAddr` *panics*; the helper exists partly for that.
2. **Never set `Transport.Proxy`** on a tunnel HTTP client.
   `http.DefaultTransport` uses `ProxyFromEnvironment`, which would send tracker
   announces to `HTTP_PROXY` over the host stack.
3. **Do not touch the fingerprint block** in `internal/engine/fingerprint.go`.
   The daemon presents as qBittorrent on purpose: `anacrolix/torrent` builds its
   identity strings from `debug.ReadBuildInfo()`, so the library defaults
   broadcast the module path to every peer. Bump the mimicked version
   deliberately; never replace it with this project's name.
4. **UDP tracker announce URLs reach the client as IP literals only.** The UDP
   tracker client re-resolves its host on every write through the host resolver
   with no hook, so hostnames are resolved in-tunnel and rewritten first. A
   hostname reaching that client is a leak.
5. **Never use `Torrent.DisallowDataDownload()`.** It marks every piece
   "ignored for requests" without updating the pending set and panics the
   process via anacrolix's own consistency check, seconds after any
   `torrent-stop`. Pause is "no piece is wanted" - see
   `internal/engine/torrents.go`.
6. **Guard every metadata-dependent call on `t.Info() != nil`.**
   `Torrent.Files()` dereferences a nil pointer before the info dict resolves,
   and clients poll immediately after adding a magnet.
7. **The config schema is a compatibility surface.** Live `config.toml` files
   exist and unknown keys are a startup error by design. Adding a key is fine;
   renaming or removing one breaks a deployment.
8. **Shutdown order is correctness, not tidiness.** HTTP first, then the torrent
   client, then flush piece completion, then the database, then the device.
   `Client.Close()` emits a final round of completions, so flushing before it
   loses exactly the pieces verified last. It lives in `session.close` once;
   every entry point goes through it rather than repeating the sequence.
9. **"Complete" means the selected files, not the torrent.**
   `Torrent.BytesMissing` counts every incomplete piece whether it is wanted or
   not, so `Status.MissingBytes` never reaches zero once any file is deselected
   - by `allowed_extensions` or by hand. Anything waiting for a download to
   finish waits on `Engine.SelectedBytes`.
