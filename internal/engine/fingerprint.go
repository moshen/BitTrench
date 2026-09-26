package engine

import (
	"crypto/rand"
	"fmt"
)

// The client we present as on the wire.
//
// Setting these is not optional. anacrolix/torrent's `version` package builds
// its defaults from debug.ReadBuildInfo() at init, so the library default
// sends this module's own path and version to *every* peer in the BEP 10
// extended handshake, and an `anacrolix-torrent/vX` user agent to every HTTP
// tracker. For a daemon whose entire purpose is not leaking identifying
// information, that is unacceptable regardless of what replaces it.
//
// Mimicking a common client rather than announcing an honest `-WG0001-` was a
// deliberate choice: private trackers whitelist known clients and an unknown
// fingerprint gets rejected. The cost is that this version must be bumped
// deliberately - a years-stale qBittorrent is as distinctive as a unique
// string. Keep the three constants in one block so they cannot drift apart.
const (
	mimicVersion = "5.1.0"
	// Azureus-style peer id prefix: -qBMMmp-.
	mimicPeerIDPrefix = "-qB5100-"
	mimicClient       = "qBittorrent/" + mimicVersion
)

// peerIDTailAlphabet is what fills the 12 bytes after the prefix.
//
// anacrolix/torrent, given only a Bep20 prefix, fills the tail with raw
// crypto/rand bytes (client.go:316-323). Real clients use printable
// characters, so a raw-byte tail is itself a fingerprint - which defeats the
// point of setting a prefix at all. This alphabet is asserted from the peer-id
// convention, not verified against a capture of a real qBittorrent; confirm it
// on the wire before relying on the mimicry for a tracker whitelist.
const peerIDTailAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// peerIDLength is fixed by BEP 3.
const peerIDLength = 20

// newPeerID builds a full 20-byte peer id: the mimicked prefix plus a random
// alphanumeric tail.
func newPeerID() (string, error) {
	tail := make([]byte, peerIDLength-len(mimicPeerIDPrefix))
	// Rejection sampling rather than a modulo: 256 is not a multiple of 62, so
	// a modulo would make the first few characters of the alphabet slightly
	// more likely. Nothing here is a secret, but a skewed tail is one more
	// thing that distinguishes us from the client being mimicked.
	const limit = 256 - 256%len(peerIDTailAlphabet)
	buf := make([]byte, 1)
	for i := range tail {
		for {
			if _, err := rand.Read(buf); err != nil {
				return "", fmt.Errorf("failed to generate a peer id: %w", err)
			}
			if int(buf[0]) < limit {
				tail[i] = peerIDTailAlphabet[int(buf[0])%len(peerIDTailAlphabet)]
				break
			}
		}
	}
	return mimicPeerIDPrefix + string(tail), nil
}
