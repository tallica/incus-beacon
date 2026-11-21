package incus

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"

	"github.com/tallica/incus-beacon/internal/policy"
	"github.com/tallica/incus-beacon/internal/registry"
)

const (
	RefFromBeacon       = policy.Prefix + "image"
	RefFromCompose      = "user.image_alias"
	RefFromUpdateSource = "update_source"
	RefFromImageKeys    = "image.description"
	RefFromApplied      = policy.Prefix + "applied"
)

// incus-compose's image override, honoured where ComposeFeaturesKey lists ComposeFeatureOverride.
const (
	ComposeOverrideKey     = "user.incus-compose.image-override"
	ComposeOverrideOfKey   = "user.incus-compose.image-override-of"
	ComposeFeaturesKey     = "user.incus-compose.features"
	ComposeFeatureOverride = "image-override"
)

type Beacon struct {
	Status  string
	Current string
	Latest  string
	Error   string
	Since   string
}

type Target struct {
	Project   string
	Instance  string
	Arch      string
	BaseImage string
	Ref       string
	RefSource string
	Applied   bool   // Ref is the ref beacon updated to, standing in for Asked
	Asked     string // the ref RefSource gave; equal to Ref unless Applied
	ImageRef  string // from the image keys, as launched or last rebuilt
	Local     bool

	ComposeProject string
	ComposeService string

	Policy    policy.Policy
	PolicyErr error
	Legacy    []string // policy keys set under their old names
	Beacon    Beacon
	Update    Update
	Claim     string // the update claim, while one holds the instance
	// LegacyStatus is set while the status keys still use their old names.
	LegacyStatus bool
	Inputs       string // the config a check reads, to tell a real change from a status write
}

type Key struct {
	Project  string
	Instance string
}

func (t Target) Key() Key { return Key{Project: t.Project, Instance: t.Instance} }

type ListOptions struct {
	Projects []string
	Instance string
	WatchAll bool
}

// Targets lists watched OCI containers; no Projects means all visible ones.
func Targets(ctx context.Context, srv incusclient.InstanceServer, opts ListOptions) ([]Target, error) {
	instances, err := listInstances(ctx, srv, opts.Projects)
	if err != nil {
		return nil, err
	}
	sources := map[string]*api.ImageSource{}
	var targets []Target
	for i := range instances {
		if opts.Instance != "" && instances[i].Name != opts.Instance {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if t, ok := target(srv, &instances[i], opts.WatchAll, sources); ok {
			targets = append(targets, t)
		}
	}
	slices.SortFunc(targets, func(a, b Target) int {
		if c := strings.Compare(a.Project, b.Project); c != 0 {
			return c
		}
		return strings.Compare(a.Instance, b.Instance)
	})
	return targets, nil
}

// TargetOf reads one instance; false means it is not a watched OCI container.
func TargetOf(ctx context.Context, srv incusclient.InstanceServer, k Key, watchAll bool) (Target, bool, error) {
	if err := ctx.Err(); err != nil {
		return Target{}, false, err
	}
	inst, _, err := srv.UseProject(k.Project).GetInstance(k.Instance)
	if err != nil {
		return Target{}, false, fmt.Errorf("get instance %s in project %s: %w", k.Instance, k.Project, err)
	}
	inst.Project = k.Project
	t, ok := target(srv, inst, watchAll, map[string]*api.ImageSource{})
	return t, ok, nil
}

func target(srv incusclient.InstanceServer, inst *api.Instance, watchAll bool, sources map[string]*api.ImageSource) (Target, bool) {
	if !isOCI(inst.ExpandedConfig) {
		return Target{}, false
	}
	pol, polErr := policy.Parse(inst.ExpandedConfig, watchAll)
	if !pol.Watched {
		return Target{}, false
	}
	t := Target{
		Project:   project(inst.Project),
		Instance:  inst.Name,
		Arch:      cmp.Or(inst.ExpandedConfig["image.architecture"], inst.Architecture),
		BaseImage: inst.Config["volatile.base_image"],
		Local:     !pulled(inst.ExpandedConfig),
		Policy:    pol,
		PolicyErr: polErr,
		Legacy:    policy.Legacy(inst.ExpandedConfig),
		Inputs:    inputs(inst),
		ImageRef:  imageKeysRef(inst.ExpandedConfig),

		ComposeProject: inst.ExpandedConfig["user.label.incus-compose.project"],
		ComposeService: inst.ExpandedConfig["user.label.incus-compose.service"],
	}
	t.Beacon, t.LegacyStatus = readBeacon(inst.Config)
	t.Update = readUpdate(inst.Config)
	t.Claim = inst.Config[ClaimKey]
	t.Ref, t.RefSource = resolveRef(srv, t, inst.ExpandedConfig, sources)
	t.Asked = t.Ref
	if t.RefSource == RefFromBeacon || t.RefSource == RefFromCompose {
		t.Ref, t.Applied = applied(t, t.Ref)
	}
	// Kept by incus-compose across a recreate that drops beacon's keys; its own terms apply.
	if c := inst.Config; c["user.incus-compose.managed"] == "true" && c[ComposeOverrideKey] != "" &&
		c[ComposeOverrideOfKey] == c[RefFromCompose] {
		t.Ref, t.Applied = c[ComposeOverrideKey], true
	}
	return t, true
}

// inputs covers every key a check may read but beacon's own; others may set the override.
func inputs(inst *api.Instance) string {
	written := Beacon{}.keys()
	maps.Copy(written, Update{}.Keys())
	written[ClaimKey] = ""
	for _, k := range legacyStatusKeys() {
		written[k] = ""
	}
	var b strings.Builder
	b.WriteString(inst.Architecture)
	for _, k := range slices.Sorted(maps.Keys(inst.ExpandedConfig)) {
		if _, ok := written[k]; ok {
			continue
		}
		if k == "volatile.base_image" || k == RefFromCompose || k == ComposeOverrideKey || k == ComposeOverrideOfKey || strings.HasPrefix(k, "image.") ||
			strings.HasPrefix(k, policy.Prefix) || strings.HasPrefix(k, policy.LabelPrefix) ||
			strings.HasPrefix(k, policy.LegacyPrefix) || strings.HasPrefix(k, policy.LegacyLabelPrefix) {
			fmt.Fprintf(&b, "\n%s=%s", k, inst.ExpandedConfig[k])
		}
	}
	return b.String()
}

func listInstances(ctx context.Context, srv incusclient.InstanceServer, projects []string) ([]api.Instance, error) {
	if len(projects) == 0 {
		insts, err := srv.GetInstancesAllProjects(api.InstanceTypeContainer)
		if err != nil {
			return nil, fmt.Errorf("list instances: %w", err)
		}
		return insts, nil
	}
	// Incus lists no instances, without an error, for a project that doesn't exist.
	known, err := srv.GetProjectNames()
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	var all []api.Instance
	for _, p := range projects {
		if !slices.Contains(known, p) {
			return nil, fmt.Errorf("project %s: not found, or this certificate may not use it", p)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		insts, err := srv.UseProject(p).GetInstances(api.InstanceTypeContainer)
		if err != nil {
			return nil, fmt.Errorf("list instances in project %s: %w", p, err)
		}
		for i := range insts {
			insts[i].Project = p
		}
		all = append(all, insts...)
	}
	return all, nil
}

// pulled matches the description Incus gives every image it pulls from a registry.
func pulled(config map[string]string) bool {
	return strings.HasSuffix(config["image.description"], " (OCI)")
}

func isOCI(config map[string]string) bool {
	return config["volatile.container.oci"] == "true" || config["image.type"] == "oci"
}

func project(name string) string {
	if name == "" {
		return api.ProjectDefaultName
	}
	return name
}

// readBeacon reads the local config: beacon keys are never set from profiles.
func readBeacon(config map[string]string) (b Beacon, legacy bool) {
	for _, k := range legacyStatusKeys() {
		legacy = legacy || config[k] != ""
	}
	prefix := policy.Prefix
	if legacy && config[prefix+"status"] == "" {
		prefix = policy.LegacyPrefix
	}
	return Beacon{
		Status:  config[prefix+"status"],
		Current: config[prefix+"current"],
		Latest:  config[prefix+"latest"],
		Error:   config[prefix+"error"],
		Since:   config[prefix+"since"],
	}, legacy
}

// applied is the ref beacon updated to, while the asked-for ref is still the one it moved from.
func applied(t Target, asked string) (string, bool) {
	if t.Update.Applied != "" && sameRef(asked, t.Update.Requested) {
		return t.Update.Applied, true
	}
	return asked, false
}

// sameRef compares refs as Docker spells them out, so nginx:1.28 is docker.io/library/nginx:1.28.
func sameRef(a, b string) bool { return registry.Normalize(a) == registry.Normalize(b) }

// resolveRef follows docs/ARCHITECTURE.md#image-ref-resolution.
func resolveRef(srv incusclient.InstanceServer, t Target, config map[string]string, sources map[string]*api.ImageSource) (ref, from string) {
	if t.Policy.Image != "" {
		return t.Policy.Image, RefFromBeacon
	}
	if v := config["user.image_alias"]; v != "" {
		return v, RefFromCompose
	}
	// Beacon's record for this exact image beats Incus's: docs/ARCHITECTURE.md#image-ref-resolution.
	if u := t.Update; u.Applied != "" && u.AppliedImage == t.BaseImage {
		return u.Applied, RefFromApplied
	}
	key := t.Project + "/" + t.BaseImage
	src, seen := sources[key]
	if !seen && t.BaseImage != "" {
		if img, _, err := srv.UseProject(t.Project).GetImage(t.BaseImage); err == nil {
			src = img.UpdateSource
		}
		sources[key] = src
	}
	if ref := sourceRef(src); ref != "" {
		return ref, RefFromUpdateSource
	}
	if ref := imageKeysRef(config); ref != "" {
		return ref, RefFromImageKeys
	}
	return "", ""
}

func imageKeysRef(config map[string]string) string {
	name, ok := strings.CutSuffix(config["image.description"], " (OCI)")
	if !ok || !strings.Contains(name, "/") {
		return ""
	}
	id := config["image.id"]
	if strings.Contains(id, "@") {
		return ""
	}
	i := strings.LastIndex(id, ":")
	if i < 0 || strings.Contains(id[i:], "/") {
		return ""
	}
	return name + id[i:]
}

// sourceRef drops a pinned @digest: the tag is what gets tracked.
func sourceRef(src *api.ImageSource) string {
	if src == nil || src.Protocol != "oci" || src.Alias == "" {
		return ""
	}
	u, err := url.Parse(src.Server)
	if err != nil || u.Host == "" {
		return ""
	}
	alias, _, _ := strings.Cut(src.Alias, "@")
	return u.Host + strings.TrimSuffix(u.Path, "/") + "/" + alias
}
