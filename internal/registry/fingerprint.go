package registry

import (
	"crypto/sha256"
	"encoding/hex"
)

// Fingerprint mirrors computeFingerprint in Incus client/oci_images.go.
func Fingerprint(layerDigests []string) string {
	h := sha256.New()
	for _, d := range layerDigests {
		h.Write([]byte(d))
	}
	return hex.EncodeToString(h.Sum(nil))
}
