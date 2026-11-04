package incus

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"

	"github.com/tallica/incus-beacon/internal/policy"
)

const (
	RefFromBeacon       = "user.beacon.image"
	RefFromCompose      = "user.image_alias"
	RefFromUpdateSource = "update_source"
	RefFromImageKeys    = "image.description"
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
	Policy    policy.Policy
	PolicyErr error
	Beacon    Beacon
}

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
		inst := &instances[i]
		if opts.Instance != "" && inst.Name != opts.Instance {
			continue
		}
		if !isOCI(inst.ExpandedConfig) {
			continue
		}
		pol, polErr := policy.Parse(inst.ExpandedConfig, opts.WatchAll)
		if !pol.Watched {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		t := Target{
			Project:   project(inst.Project),
			Instance:  inst.Name,
			Arch:      inst.ExpandedConfig["image.architecture"],
			BaseImage: inst.Config["volatile.base_image"],
			Policy:    pol,
			PolicyErr: polErr,
			Beacon:    readBeacon(inst.Config),
		}
		t.Ref, t.RefSource = resolveRef(srv, t, inst.ExpandedConfig, sources)
		targets = append(targets, t)
	}
	slices.SortFunc(targets, func(a, b Target) int {
		if c := strings.Compare(a.Project, b.Project); c != 0 {
			return c
		}
		return strings.Compare(a.Instance, b.Instance)
	})
	return targets, nil
}

func listInstances(ctx context.Context, srv incusclient.InstanceServer, projects []string) ([]api.Instance, error) {
	if len(projects) == 0 {
		insts, err := srv.GetInstancesAllProjects(api.InstanceTypeContainer)
		if err != nil {
			return nil, fmt.Errorf("list instances: %w", err)
		}
		return insts, nil
	}
	var all []api.Instance
	for _, p := range projects {
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
func readBeacon(config map[string]string) Beacon {
	return Beacon{
		Status:  config["user.beacon.status"],
		Current: config["user.beacon.current"],
		Latest:  config["user.beacon.latest"],
		Error:   config["user.beacon.error"],
		Since:   config["user.beacon.since"],
	}
}

// resolveRef follows docs/ARCHITECTURE.md#image-ref-resolution.
func resolveRef(srv incusclient.InstanceServer, t Target, config map[string]string, sources map[string]*api.ImageSource) (ref, from string) {
	if t.Policy.Image != "" {
		return t.Policy.Image, RefFromBeacon
	}
	if v := config["user.image_alias"]; v != "" {
		return v, RefFromCompose
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
