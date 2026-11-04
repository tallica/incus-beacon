package registry

import "testing"

// TestFingerprintMatchesIncus uses a public image whose volatile.base_image
// was read from a running Incus instance.
func TestFingerprintMatchesIncus(t *testing.T) {
	t.Parallel()
	layers := []string{
		"sha256:b05093807bb0294152bb9cf86d64da722732dddaf7f8882fa1f120477dbc4db3",
		"sha256:628ff3fa7c1058b67b042b43957c1cc2c4ab23dc37e4d9d12e13d08af3b1d2cd",
	}
	const want = "51ca3b785bcb111e765b2cafd7b31133f3e0330fb1fa605b4bf7a5752247b779"
	if got := Fingerprint(layers); got != want {
		t.Errorf("Fingerprint = %s, want %s", got, want)
	}
}
