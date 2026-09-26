package tunnel

import (
	"encoding/hex"
	"net/netip"
	"strings"
	"testing"
)

func TestUAPIConfigIsHexAndFullTunnel(t *testing.T) {
	var priv, pub [32]byte
	priv[0], pub[0] = 1, 2
	got := uapiConfig(priv, pub, netip.MustParseAddrPort("198.51.100.5:51820"), 25)

	for _, want := range []string{
		"private_key=" + hex.EncodeToString(priv[:]),
		"public_key=" + hex.EncodeToString(pub[:]),
		"endpoint=198.51.100.5:51820",
		"allowed_ip=0.0.0.0/0",
		"persistent_keepalive_interval=25",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("uapi config missing %q:\n%s", want, got)
		}
	}
	// 0 disables keepalive, and the key must then be absent rather than 0,
	// which is what wg-quick emits.
	if off := uapiConfig(priv, pub, netip.MustParseAddrPort("198.51.100.5:51820"), 0); strings.Contains(off, "persistent_keepalive") {
		t.Errorf("keepalive 0 should emit no key:\n%s", off)
	}
}
