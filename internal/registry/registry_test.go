package registry

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

var (
	layerA = "sha256:" + strings.Repeat("a", 64)
	layerB = "sha256:" + strings.Repeat("b", 64)
	layerC = "sha256:" + strings.Repeat("c", 64)
)

var amd64 = Platform{"linux", "amd64", ""}

func TestResolveIndex(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	amd := manifestBlob(t, ocispec.MediaTypeImageManifest, layerA, layerB)
	arm := manifestBlob(t, ocispec.MediaTypeImageManifest, layerC)
	idx := indexBlob(t, ocispec.MediaTypeImageIndex, map[string]blob{"linux/amd64": amd, "linux/arm64": arm})
	f.add("example/app", "1.2", amd)
	f.add("example/app", "1.2", arm)
	f.add("example/app", "1.2", idx)

	c := newTestClient(t, "")
	ref := mustRef(t, f.host+"/example/app:1.2")

	head, err := c.Head(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if head != idx.digest {
		t.Errorf("head %s, want the index digest %s", head, idx.digest)
	}

	img, err := c.Resolve(t.Context(), ref, amd64)
	if err != nil {
		t.Fatal(err)
	}
	if img.Digest != idx.digest || img.ManifestDigest != amd.digest {
		t.Errorf("digests %+v, want index %s and manifest %s", img, idx.digest, amd.digest)
	}
	if want := Fingerprint([]string{layerA, layerB}); img.Fingerprint != want {
		t.Errorf("fingerprint %s, want %s", img.Fingerprint, want)
	}

	arm64, err := PlatformFor("aarch64")
	if err != nil {
		t.Fatal(err)
	}
	img, err = c.Resolve(t.Context(), ref, arm64)
	if err != nil {
		t.Fatal(err)
	}
	if img.ManifestDigest != arm.digest {
		t.Errorf("arm64 with variant v8 picked %s, want the variant-less arm64 entry %s", img.ManifestDigest, arm.digest)
	}

	if _, err := c.Resolve(t.Context(), ref, Platform{"linux", "s390x", ""}); err == nil || !strings.Contains(err.Error(), "no image for") {
		t.Errorf("s390x: err %v, want no image for the platform", err)
	}
}

func TestResolveDockerManifest(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	m := manifestBlob(t, mediaTypeDockerManifest, layerC)
	f.add("library/app", "3", m)

	img, err := newTestClient(t, "").Resolve(t.Context(), mustRef(t, f.host+"/library/app:3"), amd64)
	if err != nil {
		t.Fatal(err)
	}
	if img.Digest != m.digest || img.ManifestDigest != m.digest || img.Fingerprint != Fingerprint([]string{layerC}) {
		t.Errorf("single manifest: %+v", img)
	}
}

func TestSelectPlatformPrefersExactVariant(t *testing.T) {
	t.Parallel()
	ms := []ocispec.Descriptor{
		{Digest: "sha256:loose", Platform: &ocispec.Platform{OS: "linux", Architecture: "arm64"}},
		{Digest: "sha256:exact", Platform: &ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}},
	}
	got, ok := selectPlatform(ms, Platform{"linux", "arm64", "v8"})
	if !ok || got.Digest != "sha256:exact" {
		t.Errorf("picked %s, want the exact variant", got.Digest)
	}
}

func TestNotFound(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	_, err := newTestClient(t, "").Head(t.Context(), mustRef(t, f.host+"/example/app:9"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err %v, want ErrNotFound", err)
	}
}

func TestTagsPagination(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	f.tags["example/app"] = []string{"1.0", "1.1", "1.2", "2.0", "latest"}

	tags, err := newTestClient(t, "").Tags(t.Context(), mustRef(t, f.host+"/example/app:1.0"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tags, f.tags["example/app"]) {
		t.Errorf("tags %q, want all five across three pages", tags)
	}
}

func TestAuthFromConfigJSON(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	f.auth["private/app"] = "user:secret"
	m := manifestBlob(t, ocispec.MediaTypeImageManifest, layerA)
	f.add("private/app", "1", m)
	ref := mustRef(t, f.host+"/private/app:1")

	if _, err := newTestClient(t, "").Head(t.Context(), ref); !errors.Is(err, ErrAuth) {
		t.Errorf("anonymous: err %v, want ErrAuth", err)
	}
	c := newTestClient(t, writeAuthFile(t, f.host, "user:secret"))
	head, err := c.Head(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if head != m.digest {
		t.Errorf("head %s, want %s", head, m.digest)
	}
}

func TestRateLimit429(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	f.status = http.StatusTooManyRequests
	f.header.Set("Retry-After", "120")
	c := newTestClient(t, "")
	ref := mustRef(t, f.host+"/example/app:1")

	_, err := c.Head(t.Context(), ref)
	var rl *RateLimitError
	if !errors.As(err, &rl) || !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err %v, want a RateLimitError", err)
	}
	if d := time.Until(rl.Until); d < 100*time.Second || d > 130*time.Second {
		t.Errorf("paused for %s, want about Retry-After's 120s", d)
	}

	before := f.count(http.MethodHead) + f.count(http.MethodGet)
	if _, err := c.Head(t.Context(), ref); !errors.Is(err, ErrRateLimited) {
		t.Errorf("second call: err %v, want ErrRateLimited", err)
	}
	if after := f.count(http.MethodHead) + f.count(http.MethodGet); after != before {
		t.Errorf("paused host still got %d requests", after-before)
	}
}

func TestRateLimitRemainingZeroKeepsHeads(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	m := manifestBlob(t, ocispec.MediaTypeImageManifest, layerA)
	f.add("example/app", "1", m)
	f.header.Set("Ratelimit-Limit", "100;w=21600")
	f.header.Set("Ratelimit-Remaining", "0;w=21600")
	c := newTestClient(t, "")
	ref := mustRef(t, f.host+"/example/app:1")

	if _, err := c.Head(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Head(t.Context(), ref); err != nil {
		t.Errorf("HEAD after remaining=0: %v, want it still allowed", err)
	}
	if _, err := c.Resolve(t.Context(), ref, amd64); !errors.Is(err, ErrRateLimited) {
		t.Errorf("GET after remaining=0: err %v, want ErrRateLimited", err)
	}
}

func TestRetryAfterAndWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	if got := retryAfter("30", now); got != 30*time.Second {
		t.Errorf("retryAfter seconds = %s", got)
	}
	if got := retryAfter(now.Add(5*time.Minute).Format(http.TimeFormat), now); got != 5*time.Minute {
		t.Errorf("retryAfter date = %s", got)
	}
	if got := retryAfter("", now); got != defaultBackoff {
		t.Errorf("retryAfter empty = %s", got)
	}
	if got := window("100;w=21600"); got != 6*time.Hour {
		t.Errorf("window = %s", got)
	}
	if !remainingZero("0;w=21600") || remainingZero("5;w=21600") || remainingZero("") {
		t.Error("remainingZero misread its header")
	}
}
