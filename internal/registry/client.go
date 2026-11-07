package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	orasregistry "oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/tallica/incus-beacon/internal/metrics"
)

const (
	mediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	mediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
)

const (
	maxManifestBytes = 4 << 20
	maxTags          = 10000
)

var (
	ErrAuth     = errors.New("registry auth")
	ErrNotFound = errors.New("not found")
	// ErrNoAccess: no credentials sent, and registries refuse a private and a missing repository alike.
	ErrNoAccess = errors.New("not found, or no access")
)

type Options struct {
	// AuthFile empty means $DOCKER_CONFIG or ~/.docker; missing means anonymous.
	AuthFile  string
	Timeout   time.Duration
	UserAgent string
	Metrics   *metrics.Set // nil records nothing
}

type Client struct {
	http      *http.Client
	cache     auth.Cache
	cred      auth.CredentialFunc
	header    http.Header
	timeout   time.Duration
	plainHTTP bool
}

func New(opts Options) (*Client, error) {
	var (
		store credentials.Store
		err   error
	)
	if opts.AuthFile != "" {
		store, err = credentials.NewStore(opts.AuthFile, credentials.StoreOptions{})
	} else {
		store, err = credentials.NewStoreFromDocker(credentials.StoreOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("registry credentials: %w", err)
	}
	header := http.Header{}
	if opts.UserAgent != "" {
		header.Set("User-Agent", opts.UserAgent)
	}
	transport := http.DefaultTransport
	if opts.Metrics != nil {
		transport = newObserver(transport, opts.Metrics)
	}
	return &Client{
		http:    &http.Client{Transport: newLimiter(transport)},
		cache:   auth.NewCache(),
		cred:    credentials.Credential(store),
		header:  header,
		timeout: opts.Timeout,
	}, nil
}

type Image struct {
	// Digest is the tag's top-level digest, often an index.
	Digest         string
	ManifestDigest string
	Fingerprint    string
}

// Head uses HEAD, which Docker Hub doesn't count against its pull limit.
func (c *Client) Head(ctx context.Context, ref Ref) (string, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	desc, err := c.repo(ref).Resolve(ctx, ref.Reference())
	if err != nil {
		return "", c.classify(ctx, ref, err)
	}
	return desc.Digest.String(), nil
}

func (c *Client) Resolve(ctx context.Context, ref Ref, platform Platform) (Image, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	repo := c.repo(ref)
	desc, body, err := fetch(func() (ocispec.Descriptor, io.ReadCloser, error) {
		return repo.FetchReference(ctx, ref.Reference())
	})
	if err != nil {
		return Image{}, c.classify(ctx, ref, err)
	}
	img := Image{Digest: desc.Digest.String(), ManifestDigest: desc.Digest.String()}

	switch desc.MediaType {
	case ocispec.MediaTypeImageIndex, mediaTypeDockerManifestList:
		var index ocispec.Index
		if err := json.Unmarshal(body, &index); err != nil {
			return Image{}, fmt.Errorf("%s: index: %w", ref, err)
		}
		m, ok := selectPlatform(index.Manifests, platform)
		if !ok {
			return Image{}, fmt.Errorf("%s: no image for %s", ref, platform)
		}
		desc, body, err = fetch(func() (ocispec.Descriptor, io.ReadCloser, error) {
			rc, err := repo.Fetch(ctx, m)
			return m, rc, err
		})
		if err != nil {
			return Image{}, c.classify(ctx, ref, err)
		}
		img.ManifestDigest = desc.Digest.String()
	case ocispec.MediaTypeImageManifest, mediaTypeDockerManifest:
	default:
		return Image{}, fmt.Errorf("%s: unsupported media type %s", ref, desc.MediaType)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return Image{}, fmt.Errorf("%s: manifest: %w", ref, err)
	}
	layers := make([]string, 0, len(manifest.Layers))
	for _, l := range manifest.Layers {
		layers = append(layers, l.Digest.String())
	}
	img.Fingerprint = Fingerprint(layers)
	return img, nil
}

var errEnoughTags = errors.New("enough tags")

func (c *Client) Tags(ctx context.Context, ref Ref) ([]string, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	var tags []string
	err := c.repo(ref).Tags(ctx, "", func(page []string) error {
		tags = append(tags, page...)
		if len(tags) >= maxTags {
			return errEnoughTags
		}
		return nil
	})
	if err != nil && !errors.Is(err, errEnoughTags) {
		return nil, c.classify(ctx, ref, err)
	}
	return tags, nil
}

func (c *Client) repo(ref Ref) *remote.Repository {
	return &remote.Repository{
		Reference: orasregistry.Reference{Registry: apiHost(ref.Registry), Repository: ref.Repository},
		Client: &auth.Client{
			Client:     c.http,
			Header:     c.header,
			Cache:      c.cache,
			Credential: c.cred,
		},
		PlainHTTP: c.plainHTTP,
	}
}

func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.timeout)
}

func fetch(get func() (ocispec.Descriptor, io.ReadCloser, error)) (ocispec.Descriptor, []byte, error) {
	desc, rc, err := get()
	if err != nil {
		return desc, nil, err
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes+1))
	if err != nil {
		return desc, nil, err
	}
	if len(body) > maxManifestBytes {
		return desc, nil, fmt.Errorf("manifest over %d bytes", maxManifestBytes)
	}
	return desc, body, nil
}

// selectPlatform prefers an exact variant match over a lenient one.
func selectPlatform(manifests []ocispec.Descriptor, p Platform) (ocispec.Descriptor, bool) {
	var lenient *ocispec.Descriptor
	for i := range manifests {
		m := &manifests[i]
		if !p.matches(m.Platform) {
			continue
		}
		if m.Platform.Variant == p.Variant {
			return *m, true
		}
		if lenient == nil {
			lenient = m
		}
	}
	if lenient != nil {
		return *lenient, true
	}
	return ocispec.Descriptor{}, false
}

func (c *Client) refused(ctx context.Context, ref Ref) error {
	cred, err := c.cred(context.WithoutCancel(ctx), apiHost(ref.Registry))
	if err == nil && cred == auth.EmptyCredential {
		return ErrNoAccess
	}
	return ErrAuth
}

func (c *Client) classify(ctx context.Context, ref Ref, err error) error {
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return rl
	}
	var resp *errcode.ErrorResponse
	if errors.As(err, &resp) {
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("%s: %w", ref, c.refused(ctx, ref))
		case http.StatusNotFound:
			return fmt.Errorf("%s: %w", ref, ErrNotFound)
		}
	}
	if errors.Is(err, auth.ErrBasicCredentialNotFound) {
		return fmt.Errorf("%s: %w", ref, ErrNoAccess)
	}
	if errors.Is(err, errdef.ErrNotFound) {
		return fmt.Errorf("%s: %w", ref, ErrNotFound)
	}
	return fmt.Errorf("%s: %w", ref, err)
}
