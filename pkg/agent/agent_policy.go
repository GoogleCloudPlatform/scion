// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// The operator's agent policy (config.V1AgentPolicy, the global settings
// file's agent_policy block) is enforced by Start on the final RunConfig,
// immediately before Runtime.Run (enforceAgentPolicy). Every value it judges
// has been fully resolved by then (settings, profile, harness config,
// template, the agent's stored config, inline config and the Hub request
// all merged), and Run is the only place any runtime creates an agent
// container, so no source can get around it. The image is also checked
// before it is pulled (checkAgentPolicyImage).

// ErrAgentPolicy is wrapped by every agent policy refusal.
var ErrAgentPolicy = errors.New("refused by agent policy")

// AgentPolicyError is a start refused by the agent policy. Its message names
// the agent config field and the policy setting, and repeats only the
// requester's own value (bounded to maxEchoedPolicyValue), never a resolved
// host path or the policy's allow lists, so it is safe to return to the
// requester.
type AgentPolicyError struct {
	// Field is the agent config field refused (e.g. "image",
	// "resources.limits.cpu").
	Field string
	// Setting is the policy setting that refused it (e.g.
	// "agent_policy.allowed_image_prefixes").
	Setting string
	msg     string
}

// NewAgentPolicyError returns a refusal of field by the policy setting. The
// formatted reason must follow PublicMessage's rules.
func NewAgentPolicyError(field, setting, format string, args ...any) *AgentPolicyError {
	return &AgentPolicyError{
		Field:   field,
		Setting: setting,
		msg:     fmt.Sprintf("agent policy: %s: %s (%s)", field, fmt.Sprintf(format, args...), setting),
	}
}

func (e *AgentPolicyError) Error() string { return e.msg }

// PublicMessage returns the text the requester may see.
func (e *AgentPolicyError) PublicMessage() string { return e.msg }

// Unwrap exposes ErrAgentPolicy.
func (e *AgentPolicyError) Unwrap() error { return ErrAgentPolicy }

// loadAgentPolicy loads the agent policy for a start. A variable so tests
// can substitute a policy.
var loadAgentPolicy = config.LoadAgentPolicy

// loadStartAgentPolicy loads the agent policy, failing the start closed
// when it cannot be loaded or is invalid. The detail goes to the log; the
// requester sees a fixed message.
func loadStartAgentPolicy(agentName string) (*config.V1AgentPolicy, error) {
	p, err := loadAgentPolicy()
	if err != nil {
		slog.Warn("Agent policy could not be loaded; refusing the start", "agent", agentName, "error", err)
		return nil, NewAgentPolicyError("agent", "agent_policy", "the host's agent policy could not be loaded; ask the operator to check the agent_policy settings")
	}
	return p, nil
}

// refuseAgentPolicy logs a refusal at WARN and returns it.
func refuseAgentPolicy(agentName string, e *AgentPolicyError) error {
	slog.Warn("Agent policy refused agent start", "agent", agentName, "field", e.Field, "setting", e.Setting)
	return e
}

// normalizeImageReference returns ref fully qualified the way Docker and
// containerd read it: a reference whose first component is not a registry
// host (no "." or ":" and not "localhost") is on docker.io, and a
// single-component docker.io repository is under library/.
func normalizeImageReference(ref string) string {
	ref = strings.TrimSpace(ref)
	first, rest, hasSlash := strings.Cut(ref, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		// Registry host names are case-insensitive.
		first = strings.ToLower(first)
		if first == "index.docker.io" {
			first = "docker.io"
		}
		ref = first + "/" + rest
		if first == "docker.io" && !strings.Contains(rest, "/") {
			ref = "docker.io/library/" + rest
		}
		return ref
	}
	if !hasSlash {
		return "docker.io/library/" + ref
	}
	return "docker.io/" + ref
}

// normalizeImagePrefix normalizes an allowed_image_prefixes entry the same
// way as an image reference. A bare registry host ("registry.example.com")
// or a host with a trailing "/" is kept as a host prefix.
func normalizeImagePrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	host, rest, hasSlash := strings.Cut(prefix, "/")
	isHost := strings.ContainsAny(host, ".:") || host == "localhost"
	if isHost && (!hasSlash || rest == "") {
		host = strings.ToLower(host)
		if host == "index.docker.io" {
			host = "docker.io"
		}
		return host + "/"
	}
	if strings.HasSuffix(prefix, "/") {
		// A namespace prefix ("myorg/", "docker.io/myorg/"): only the
		// registry is implied, never library/, which applies to a
		// single-component repository name only.
		if isHost {
			host = strings.ToLower(host)
			if host == "index.docker.io" {
				host = "docker.io"
			}
			return host + "/" + rest
		}
		return "docker.io/" + prefix
	}
	return normalizeImageReference(prefix)
}

// imageMatchesPrefix reports whether the normalized image starts with the
// normalized prefix at a reference boundary: a prefix ending in "/" matches
// anything under it; any other prefix matches the reference itself or one
// continued by "/", ":" or "@" (so registry/path never matches
// registry/path-evil).
func imageMatchesPrefix(image, prefix string) bool {
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(image, prefix)
	}
	if !strings.HasPrefix(image, prefix) {
		return false
	}
	if len(image) == len(prefix) {
		return true
	}
	switch image[len(prefix)] {
	case '/', ':', '@':
		return true
	}
	return false
}

// checkAgentPolicyImage checks image against allowed_image_prefixes and
// returns the reference to use: the fully qualified form when a prefix list
// is set, so the image checked is the image pulled and run whatever the
// runtime's short-name resolution, else image unchanged.
func checkAgentPolicyImage(p *config.V1AgentPolicy, agentName, image string) (string, error) {
	if p == nil || len(p.AllowedImagePrefixes) == 0 {
		return image, nil
	}
	if strings.TrimSpace(image) == "" {
		return "", refuseAgentPolicy(agentName, NewAgentPolicyError("image", "agent_policy.allowed_image_prefixes", "no image is set"))
	}
	normalized := normalizeImageReference(image)
	for _, prefix := range p.AllowedImagePrefixes {
		if imageMatchesPrefix(normalized, normalizeImagePrefix(prefix)) {
			return normalized, nil
		}
	}
	return "", refuseAgentPolicy(agentName, NewAgentPolicyError("image", "agent_policy.allowed_image_prefixes",
		"image %s is not from an allowed registry or repository", quotePolicyValue(config.RedactImageRegistry(image))))
}

// maxEchoedPolicyValue is how many characters of a requester's value an
// agent policy error repeats.
const maxEchoedPolicyValue = 128

func quotePolicyValue(v string) string {
	if len(v) > maxEchoedPolicyValue {
		v = v[:maxEchoedPolicyValue] + "..."
	}
	return strconv.Quote(v)
}

// k8sDefaultRequestFor is the request a Kubernetes container gets for a
// resource with neither a request nor a limit (runtime
// k8sDefaultResourceRequests). When the policy fills a missing limit on
// Kubernetes, the request is filled with this value (at most the maximum),
// so the API server does not default the request to the filled limit.
var k8sDefaultRequestFor = map[string]string{"cpu": "250m", "memory": "512Mi"}

// fillMissingLimitRuntimes are the runtimes where omitting a resource limit
// leaves the container unlimited (or where the runtime reads only Limits),
// so max_resources fills missing limits with the configured maximum. Runtimes
// that fall back from limit to request or apply a platform default (Cloud
// Run, Apple container) are not filled so request-only agents are not
// silently upsized and Cloud Run CPU/memory bands are not violated; instead,
// unset resources whose runtime default exceeds (or may exceed) max_resources
// — and any unknown/unlisted runtime with unset capped resources — are
// refused fail-closed.
var fillMissingLimitRuntimes = map[string]bool{
	"docker":     true,
	"podman":     true,
	"kubernetes": true,
	"substrate":  true,
}

// runtimeUnsetDefaults records the known platform defaults applied when a
// non-fill runtime receives neither a limit nor a request for a resource
// (see pkg/runtime/cloudrun_resources.go and pkg/runtime/apple_container.go).
// An empty string means the runtime omits the resource flag altogether and
// delegates to an external CLI default that may exceed max_resources, so an
// unset resource is refused whenever max_resources caps it.
var runtimeUnsetDefaults = map[string]map[string]string{
	"cloudrun":  {"cpu": "2", "memory": "2Gi"},
	"container": {"cpu": "", "memory": "2G"},
}

// applyAgentPolicyResources checks the agent's CPU and memory requests and
// limits (the common resource spec and the Kubernetes resources map, which
// overrides it on Kubernetes) against max_resources, and returns the spec to
// run with: a copy with each missing limit set to the maximum. spec is
// never modified.
func applyAgentPolicyResources(p *config.V1AgentPolicy, agentName, runtimeName string, spec *api.ResourceSpec, k8s *api.KubernetesConfig) (*api.ResourceSpec, error) {
	if p == nil || p.MaxResources == nil || (p.MaxResources.CPU == "" && p.MaxResources.Memory == "") {
		return spec, nil
	}
	out := &api.ResourceSpec{}
	if spec != nil {
		*out = *spec
	}
	var k8sRes *api.K8sResources
	if k8s != nil {
		k8sRes = k8s.Resources
	}
	type resourceRule struct {
		name       string
		max        string
		maxOf      func(string) (float64, error)
		valueOf    func(string) (float64, error)
		minValueOf func(string) (float64, error)
		// minLimit is the smallest limit every runtime reads as a limit
		// (a 0 limit is "unlimited" to Docker).
		minLimit float64
		limit    *string
		request  *string
	}
	rules := []resourceRule{
		{"cpu", p.MaxResources.CPU, config.AgentPolicyCPUMax, config.AgentPolicyCPUValue, config.AgentPolicyCPUMinValue, 0.001, &out.Limits.CPU, &out.Requests.CPU},
		{"memory", p.MaxResources.Memory, config.AgentPolicyMemoryMax, config.AgentPolicyMemoryValue, config.AgentPolicyMemoryMinValue, 1, &out.Limits.Memory, &out.Requests.Memory},
	}
	type resolvedRule struct {
		rule          resourceRule
		maxVal        float64
		k8sLimit      string
		k8sRequest    string
		hasK8sLimit   bool
		hasK8sRequest bool
	}
	resolved := make([]resolvedRule, 0, len(rules))
	// Pass 1: validate every explicit request and limit across both CPU and
	// memory so an explicit violation is always reported before unset-default
	// checks.
	for _, r := range rules {
		if r.max == "" {
			continue
		}
		maxVal, err := r.maxOf(r.max)
		if err != nil {
			// Validate refuses an invalid policy before this runs.
			return nil, refuseAgentPolicy(agentName, NewAgentPolicyError("resources", "agent_policy.max_resources."+r.name, "the policy maximum is invalid"))
		}
		check := func(field, value string) error {
			if value == "" {
				return nil
			}
			v, err := r.valueOf(value)
			if err != nil {
				return refuseAgentPolicy(agentName, NewAgentPolicyError(field, "agent_policy.max_resources."+r.name,
					"%s is not a valid quantity", quotePolicyValue(value)))
			}
			if v > maxVal {
				return refuseAgentPolicy(agentName, NewAgentPolicyError(field, "agent_policy.max_resources."+r.name,
					"%s is above the maximum %s", quotePolicyValue(value), r.max))
			}
			// A zero limit is "unlimited" to Docker (--cpus 0, --memory 0),
			// so every runtime's reading of a limit must be a real limit;
			// a negative value is never meaningful.
			minV, _ := r.minValueOf(value)
			isLimit := strings.Contains(field, "limits.")
			if minV < 0 || (isLimit && !(minV >= r.minLimit)) {
				return refuseAgentPolicy(agentName, NewAgentPolicyError(field, "agent_policy.max_resources."+r.name,
					"%s must be a positive quantity at most %s", quotePolicyValue(value), r.max))
			}
			return nil
		}
		if err := check("resources.limits."+r.name, *r.limit); err != nil {
			return nil, err
		}
		if err := check("resources.requests."+r.name, *r.request); err != nil {
			return nil, err
		}
		var k8sLimit, k8sRequest string
		var hasK8sLimit, hasK8sRequest bool
		if k8sRes != nil {
			k8sLimit, hasK8sLimit = k8sRes.Limits[r.name]
			k8sRequest, hasK8sRequest = k8sRes.Requests[r.name]
		}
		if hasK8sLimit {
			if err := check("kubernetes.resources.limits."+r.name, k8sLimit); err != nil {
				return nil, err
			}
		}
		if hasK8sRequest {
			if err := check("kubernetes.resources.requests."+r.name, k8sRequest); err != nil {
				return nil, err
			}
		}
		// Only the Kubernetes runtime reads the kubernetes.resources map;
		// every other runtime runs with the common spec alone, so there the
		// map is checked above but never stands in for a missing limit.
		if runtimeName != "kubernetes" {
			hasK8sLimit, hasK8sRequest = false, false
		}
		resolved = append(resolved, resolvedRule{
			rule:          r,
			maxVal:        maxVal,
			k8sLimit:      k8sLimit,
			k8sRequest:    k8sRequest,
			hasK8sLimit:   hasK8sLimit,
			hasK8sRequest: hasK8sRequest,
		})
	}
	// Pass 2: fill missing limits on unlimited-by-default runtimes, or refuse
	// unset resources on non-fill and unknown runtimes when the runtime
	// default exceeds (or may exceed) the maximum.
	filled := false
	for _, rr := range resolved {
		r := rr.rule
		if fillMissingLimitRuntimes[runtimeName] {
			if *r.limit == "" && (!rr.hasK8sLimit || rr.k8sLimit == "") {
				*r.limit = r.max
				filled = true
				if runtimeName == "kubernetes" && *r.request == "" && (!rr.hasK8sRequest || rr.k8sRequest == "") {
					req := k8sDefaultRequestFor[r.name]
					if v, err := r.valueOf(req); err != nil || v > rr.maxVal {
						req = r.max
					}
					*r.request = req
				}
			}
		} else if *r.limit == "" && *r.request == "" {
			if defs, ok := runtimeUnsetDefaults[runtimeName]; ok {
				if def, tracked := defs[r.name]; tracked && def != "" {
					if defVal, err := r.valueOf(def); err != nil || defVal > rr.maxVal {
						return nil, refuseAgentPolicy(agentName, NewAgentPolicyError("resources.limits."+r.name, "agent_policy.max_resources."+r.name,
							"is unset and this runtime's default (%s) exceeds the maximum %s; set it explicitly", def, r.max))
					}
					continue
				}
			}
			return nil, refuseAgentPolicy(agentName, NewAgentPolicyError("resources.limits."+r.name, "agent_policy.max_resources."+r.name,
				"is unset and this runtime's default may exceed the maximum %s; set it explicitly", r.max))
		}
	}
	if spec == nil && !filled {
		return nil, nil
	}
	return out, nil
}

// enforceAgentPolicy applies the agent policy to the final RunConfig, just
// before Runtime.Run. Refusals come first, so a refused start changes
// nothing; then the image is pinned to its fully qualified form and missing
// resource limits are filled.
func enforceAgentPolicy(p *config.V1AgentPolicy, agentName, runtimeName string, cfg *runtime.RunConfig) error {
	if p == nil {
		return nil
	}
	image, err := checkAgentPolicyImage(p, agentName, cfg.Image)
	if err != nil {
		return err
	}
	resources, err := applyAgentPolicyResources(p, agentName, runtimeName, cfg.Resources, cfg.Kubernetes)
	if err != nil {
		return err
	}
	cfg.Image = image
	cfg.Resources = resources
	return nil
}
