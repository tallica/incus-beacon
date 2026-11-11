// Package check decides, per watched instance, whether a newer image exists.
package check

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/incus"
	"github.com/tallica/incus-beacon/internal/policy"
	"github.com/tallica/incus-beacon/internal/registry"
	"github.com/tallica/incus-beacon/internal/semver"
)

type Status string

const (
	Current         Status = "current"
	UpdateAvailable Status = "update-available"
	Error           Status = "error"
	Unknown         Status = "unknown"
)

type Registry interface {
	Head(ctx context.Context, ref registry.Ref) (string, error)
	Resolve(ctx context.Context, ref registry.Ref, p registry.Platform) (registry.Image, error)
	Tags(ctx context.Context, ref registry.Ref) ([]string, error)
}

type Result struct {
	Target  incus.Target
	Status  Status
	Current string
	Latest  string
	Reason  string
}

// Engine caches across runs: an unmoved HEAD digest skips the manifest GET.
type Engine struct {
	reg         Registry
	concurrency int
	log         zerolog.Logger

	mu    sync.Mutex
	cache map[lookupKey]registry.Image
}

func New(reg Registry, concurrency int, log zerolog.Logger) *Engine {
	return &Engine{
		reg:         reg,
		concurrency: max(concurrency, 1),
		log:         log,
		cache:       map[lookupKey]registry.Image{},
	}
}

type lookupKey struct {
	ref      string
	platform string
}

type plan struct {
	target   incus.Target
	ref      registry.Ref
	platform registry.Platform
	result   *Result
}

type digestOutcome struct {
	img registry.Image
	err error
}

type tagsOutcome struct {
	tags []string
	err  error
}

func (e *Engine) Run(ctx context.Context, targets []incus.Target) []Result {
	results := make([]Result, len(targets))
	var plans []plan
	digests := map[lookupKey]*digestOutcome{}
	tagLists := map[string]*tagsOutcome{}

	for i := range targets {
		r := &results[i]
		*r = Result{Target: targets[i]}
		p, ok := prepare(r)
		if !ok {
			continue
		}
		plans = append(plans, p)
		digests[keyOf(p)] = nil
		if p.target.Policy.Track == policy.TrackSemver {
			tagLists[p.ref.Name()] = nil
		}
	}

	e.lookup(ctx, plans, digests, tagLists)

	for i := range plans {
		p := &plans[i]
		decide(p, digests[keyOf(*p)], tagLists[p.ref.Name()])
		e.log.Debug().
			Str("project", p.target.Project).Str("instance", p.target.Instance).
			Str("ref", p.result.Current).Str("platform", p.platform.String()).
			Str("registry", p.ref.Registry).Str("status", string(p.result.Status)).
			Str("latest", p.result.Latest).Str("reason", p.result.Reason).
			Msg("checked")
	}
	return results
}

// Precheck is what a check decides before any registry lookup.
func Precheck(t incus.Target) Result {
	r := Result{Target: t}
	prepare(&r)
	return r
}

func prepare(r *Result) (plan, bool) {
	t := r.Target
	r.Current = t.Ref
	switch {
	case t.PolicyErr != nil:
		r.Status, r.Reason = Error, "policy: "+t.PolicyErr.Error()
		return plan{}, false
	case t.Local && t.RefSource != incus.RefFromBeacon:
		r.Status, r.Reason = Unknown, "not pulled from a registry"
		return plan{}, false
	case t.Ref == "":
		r.Status, r.Reason = Unknown, "no registry ref"
		return plan{}, false
	case t.BaseImage == "":
		r.Status, r.Reason = Unknown, "no base image fingerprint"
		return plan{}, false
	}
	ref, err := registry.ParseRef(t.Ref)
	if err != nil {
		r.Status, r.Reason = Error, err.Error()
		return plan{}, false
	}
	r.Current = ref.String()
	if ref.Tag == "" {
		r.Status, r.Reason = Unknown, "pinned by digest"
		return plan{}, false
	}
	if reason := composeDrift(t, ref); reason != "" {
		r.Status, r.Reason = Error, reason
		return plan{}, false
	}
	platform, err := registry.PlatformFor(t.Arch)
	if err != nil {
		r.Status, r.Reason = Error, err.Error()
		return plan{}, false
	}
	return plan{target: t, ref: ref, platform: platform, result: r}, true
}

// composeDrift follows docs/ARCHITECTURE.md#image-ref-resolution.
func composeDrift(t incus.Target, ref registry.Ref) string {
	if t.RefSource != incus.RefFromCompose || t.ImageRef == "" {
		return ""
	}
	got, err := registry.ParseRef(t.ImageRef)
	if err != nil || (got.Repository == ref.Repository && got.Tag == ref.Tag) {
		return ""
	}
	runs, want := got.Tag, ref.Tag
	if got.Repository != ref.Repository {
		runs, want = got.Repository+":"+got.Tag, ref.Repository+":"+ref.Tag
	}
	return "compose drift: runs " + runs + ", compose file has " + want
}

func keyOf(p plan) lookupKey {
	return lookupKey{ref: p.ref.String(), platform: p.platform.String()}
}

func (e *Engine) lookup(ctx context.Context, plans []plan, digests map[lookupKey]*digestOutcome, tagLists map[string]*tagsOutcome) {
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		sem = make(chan struct{}, e.concurrency)
	)
	run := func(f func()) {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			f()
		})
	}
	started := map[lookupKey]bool{}
	for _, p := range plans {
		k := keyOf(p)
		if started[k] {
			continue
		}
		started[k] = true
		run(func() {
			img, err := e.image(ctx, p.ref, p.platform, k)
			mu.Lock()
			digests[k] = &digestOutcome{img: img, err: err}
			mu.Unlock()
		})
	}
	repos := map[string]registry.Ref{}
	for _, p := range plans {
		if _, wanted := tagLists[p.ref.Name()]; wanted {
			repos[p.ref.Name()] = p.ref
		}
	}
	for name, ref := range repos {
		run(func() {
			tags, err := e.reg.Tags(ctx, ref)
			mu.Lock()
			tagLists[name] = &tagsOutcome{tags: tags, err: err}
			mu.Unlock()
		})
	}
	wg.Wait()
}

func (e *Engine) image(ctx context.Context, ref registry.Ref, p registry.Platform, k lookupKey) (registry.Image, error) {
	head, err := e.reg.Head(ctx, ref)
	if err != nil {
		return registry.Image{}, err
	}
	e.mu.Lock()
	cached, ok := e.cache[k]
	e.mu.Unlock()
	if ok && cached.Digest == head {
		return cached, nil
	}
	img, err := e.reg.Resolve(ctx, ref, p)
	if err != nil {
		return registry.Image{}, err
	}
	e.mu.Lock()
	e.cache[k] = img
	e.mu.Unlock()
	return img, nil
}

func decide(p *plan, d *digestOutcome, tags *tagsOutcome) {
	r := p.result
	if d.err != nil {
		r.Status, r.Reason = Error, reason(d.err)
		return
	}
	r.Status = Current
	if d.img.Fingerprint != p.target.BaseImage {
		r.Status, r.Latest = UpdateAvailable, p.ref.Tag+"@"+short(d.img.Digest)
	}
	if p.target.Policy.Track != policy.TrackSemver {
		return
	}
	if tags.err != nil {
		r.Status, r.Latest, r.Reason = Error, "", reason(tags.err)
		return
	}
	pol := p.target.Policy
	newest, ok := semver.Newest(p.ref.Tag, tags.tags, semver.Filter{
		Include: pol.Include, Exclude: pol.Exclude, Constraint: pol.Constraint,
	})
	switch {
	case !ok:
		r.Status, r.Latest, r.Reason = Error, "", "track=semver needs a version tag, not "+p.ref.Tag
	case newest != "":
		r.Status, r.Latest = UpdateAvailable, newest
	}
}

func reason(err error) string {
	var rl *registry.RateLimitError
	switch {
	case errors.As(err, &rl):
		return "rate limited until " + rl.Until.UTC().Format(time.RFC3339)
	case errors.Is(err, registry.ErrAuth):
		return "registry auth failed"
	case errors.Is(err, registry.ErrNoAccess):
		return "not found, or private: no registry credentials"
	case errors.Is(err, registry.ErrNotFound):
		return "tag not found"
	case errors.Is(err, context.DeadlineExceeded):
		return "registry timeout"
	}
	return err.Error()
}

// short is the first 12 hex digits, as Incus prints fingerprints.
func short(digest string) string {
	_, hex, _ := strings.Cut(digest, ":")
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return "sha256:" + hex
}

// Beacon moves since only when status or latest changes.
func Beacon(r Result, now time.Time) incus.Beacon {
	prev := r.Target.Beacon
	b := incus.Beacon{Status: string(r.Status), Current: r.Current, Latest: r.Latest}
	if r.Status == Error || r.Status == Unknown {
		b.Error = r.Reason
		if b.Latest == "" {
			b.Latest = prev.Latest
		}
	}
	b.Since = prev.Since
	if b.Status != prev.Status || b.Latest != prev.Latest || b.Since == "" {
		b.Since = now.UTC().Format(time.RFC3339)
	}
	return b
}
