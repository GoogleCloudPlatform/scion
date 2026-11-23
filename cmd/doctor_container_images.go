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

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

const (
	ociImageVersionLabel  = "org.opencontainers.image.version"
	ociImageRevisionLabel = "org.opencontainers.image.revision"
)

type containerImagesDoctorDeps struct {
	lookPath     func(file string) (string, error)
	runCmd       func(ctx context.Context, name string, args ...string) ([]byte, error)
	localVersion string
	localCommit  string
}

func defaultContainerImagesDoctorDeps() containerImagesDoctorDeps {
	return containerImagesDoctorDeps{
		lookPath: exec.LookPath,
		runCmd: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		},
		localVersion: version.Version,
		localCommit:  version.Commit,
	}
}

// checkDoctorContainerImagesHere performs D7 (local container images and
// image skew checks) using the active project's effective settings and
// on-disk harness-config directories when available, falling back to
// globalSettings.
func checkDoctorContainerImagesHere(globalSettings *config.VersionedSettings) []scionruntime.CheckResult {
	var projectDir string
	vs := globalSettings
	if resolved, err := resolveActiveProjectPath(); err == nil {
		if dir, dirErr := config.GetResolvedProjectDir(resolved); dirErr == nil {
			projectDir = dir
			if eff, _, effErr := config.LoadEffectiveSettings(projectDir); effErr == nil && eff != nil {
				vs = eff
			}
		}
	}

	var activeRuntime string
	if vs != nil {
		if _, rtType, err := vs.ResolveRuntime(profile); err == nil {
			activeRuntime = rtType
		}
	}

	configuredImages := doctorConfiguredHarnessImages(projectDir, vs, profile, version.Version)
	return checkDoctorContainerImages(activeRuntime, configuredImages, defaultContainerImagesDoctorDeps())
}

// doctorConfiguredHarnessImages collects the deduplicated container image
// references configured across on-disk harness-config directories
// (~/.scion/harness-configs and <projectDir>/harness-configs) and settings
// (harness_configs, profile harness_overrides, and flat broker
// agent_defaults.harness_overrides), with image_registry and image_tag
// rewrites applied.
func doctorConfiguredHarnessImages(projectDir string, vs *config.VersionedSettings, profileName, binaryVersion string) []string {
	baseImages := make(map[string]string)
	if dirs, err := config.ListHarnessConfigDirs(projectDir); err == nil {
		for _, d := range dirs {
			if d != nil && d.Name != "" {
				baseImages[d.Name] = strings.TrimSpace(d.Config.Image)
			}
		}
	}

	namesSet := make(map[string]struct{}, len(baseImages))
	for name := range baseImages {
		namesSet[name] = struct{}{}
	}
	if vs != nil {
		for name := range vs.HarnessConfigs {
			namesSet[name] = struct{}{}
		}
		effectiveProfile := profileName
		if effectiveProfile == "" {
			effectiveProfile = vs.ActiveProfile
		}
		if p, ok := vs.Profiles[effectiveProfile]; ok {
			for name := range p.HarnessOverrides {
				namesSet[name] = struct{}{}
			}
		}
	}

	names := make([]string, 0, len(namesSet))
	for name := range namesSet {
		names = append(names, name)
	}
	sort.Strings(names)

	imageRegistry := config.ResolveImageRegistryLayers(
		vs.ImageRegistryLayers(profileName, config.ImageRegistryFromEnv())).Value
	imageTagSetting := config.ResolveImageTagLayers(
		vs.ImageTagLayers(profileName, config.ImageTagFromEnv())).Value
	imageTag, _ := config.EffectiveImageTag(imageTagSetting, binaryVersion)

	seen := make(map[string]struct{})
	addImageWithRewrites := func(img, reg, tag string) {
		img = strings.TrimSpace(img)
		if img == "" {
			return
		}
		seen[img] = struct{}{}
		if reg != "" {
			seen[config.RewriteImageRegistry(img, reg)] = struct{}{}
		}
		if tag != "" {
			seen[config.RewriteImageTag(img, tag)] = struct{}{}
		}
		if reg != "" || tag != "" {
			seen[config.RewriteImageRef(img, reg, tag)] = struct{}{}
		}
	}

	for _, name := range names {
		img := baseImages[name]
		if vs != nil {
			if hc, err := vs.ResolveHarnessConfig(profileName, name); err == nil && strings.TrimSpace(hc.Image) != "" {
				img = strings.TrimSpace(hc.Image)
			}
		}
		addImageWithRewrites(img, imageRegistry, imageTag)
	}

	// Also pick up flat broker instance defaults (server.broker.instances) for
	// local Docker targets when present.
	if vs != nil && vs.Server != nil && vs.Server.Broker != nil {
		for _, inst := range vs.Server.Broker.Instances {
			if inst.AgentDefaults == nil {
				continue
			}
			if inst.RuntimeTarget != nil && isNonLocalDoctorRuntime(strings.ToLower(strings.TrimSpace(inst.RuntimeTarget.Type))) {
				continue
			}
			instReg := imageRegistry
			if r := strings.TrimSpace(inst.AgentDefaults.ImageRegistry); r != "" {
				instReg = r
			}
			for _, override := range inst.AgentDefaults.HarnessOverrides {
				addImageWithRewrites(override.Image, instReg, imageTag)
			}
		}
	}

	out := make([]string, 0, len(seen))
	for ref := range seen {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

func isNonLocalDoctorRuntime(rt string) bool {
	return rt == "kubernetes" || rt == "k8s" || strings.HasPrefix(rt, "cloudrun")
}

// checkDoctorContainerImages performs D7: Container images check (local) and
// inspects local Docker/Podman images matching configured harness images for
// sciontool capability and OCI version/revision skew.
func checkDoctorContainerImages(activeRuntime string, configuredImages []string, deps containerImagesDoctorDeps) []scionruntime.CheckResult {
	defaults := defaultContainerImagesDoctorDeps()
	lookPath := deps.lookPath
	if lookPath == nil {
		lookPath = defaults.lookPath
	}
	runCmd := deps.runCmd
	if runCmd == nil {
		runCmd = defaults.runCmd
	}

	rt := strings.ToLower(strings.TrimSpace(activeRuntime))
	if rt == "container" || rt == "apple-container" {
		return []scionruntime.CheckResult{{
			Name:    "container-images",
			Status:  "skip",
			Message: "Container image inspection skipped for Apple Container runtime",
		}}
	}

	var containerCLI string
	switch rt {
	case "docker", "podman":
		if _, err := lookPath(rt); err == nil {
			containerCLI = rt
		} else {
			return []scionruntime.CheckResult{{
				Name:    "container-images",
				Status:  "skip",
				Message: fmt.Sprintf("Container runtime CLI %q not found in PATH", rt),
			}}
		}
	default:
		for _, cli := range []string{"docker", "podman"} {
			if _, err := lookPath(cli); err == nil {
				containerCLI = cli
				break
			}
		}
		if containerCLI == "" {
			return []scionruntime.CheckResult{{
				Name:    "container-images",
				Status:  "skip",
				Message: "No container runtime CLI found (docker/podman)",
			}}
		}
	}

	imgCtx, imgCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer imgCancel()
	out, err := runCmd(imgCtx, containerCLI, "images", "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return []scionruntime.CheckResult{{
			Name:    "container-images",
			Status:  "skip",
			Message: fmt.Sprintf("Could not list %s images: %v", containerCLI, err),
		}}
	}

	imageOutput := strings.TrimSpace(string(out))
	if imageOutput == "" {
		return []scionruntime.CheckResult{{
			Name:        "container-images",
			Status:      "warn",
			Message:     fmt.Sprintf("No scion container images found via %s", containerCLI),
			Remediation: "Pull scion images or check image_registry configuration",
		}}
	}

	images := strings.Split(imageOutput, "\n")
	scionImageCount := 0
	for _, img := range images {
		if strings.Contains(img, "scion") {
			scionImageCount++
		}
	}

	if scionImageCount == 0 {
		return []scionruntime.CheckResult{{
			Name:        "container-images",
			Status:      "warn",
			Message:     fmt.Sprintf("No scion container images found via %s", containerCLI),
			Remediation: "Pull scion images or check image_registry configuration",
		}}
	}

	results := []scionruntime.CheckResult{{
		Name:    "container-images",
		Status:  "pass",
		Message: fmt.Sprintf("%d scion image(s) found via %s", scionImageCount, containerCLI),
	}}

	// Non-local runtimes (Kubernetes, Cloud Run) pull images remotely rather
	// than using local Docker/Podman images, so skip local image skew checks.
	if isNonLocalDoctorRuntime(rt) {
		return results
	}

	matched := matchLocalHarnessImages(images, configuredImages, containerCLI)
	for _, img := range matched {
		inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 10*time.Second)
		inspectOut, inspectErr := runCmd(inspectCtx, containerCLI, "image", "inspect", "--format", "{{json .Config.Labels}}", img)
		inspectCancel()
		if inspectErr != nil {
			continue
		}

		labels, ok := parseDoctorImageLabels(inspectOut)
		if !ok {
			continue
		}

		if capRes, warn := checkDoctorImageCapabilities(img, labels); warn {
			results = append(results, capRes)
		}
		if verRes, info := checkDoctorImageVersionSkew(img, labels, deps.localVersion, deps.localCommit); info {
			results = append(results, verRes)
		}
	}

	return results
}

func parseDoctorImageLabels(raw []byte) (map[string]string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, true
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(trimmed), &labels); err != nil {
		return nil, false
	}
	return labels, true
}

func checkDoctorImageCapabilities(img string, labels map[string]string) (scionruntime.CheckResult, bool) {
	if scionruntime.ImageHasCapability(labels, scionruntime.SciontoolCapNetnsSetup) {
		return scionruntime.CheckResult{}, false
	}
	rawCap := strings.TrimSpace(labels[scionruntime.ImageSciontoolCapabilitiesLabel])
	var msg string
	if rawCap == "" {
		msg = fmt.Sprintf(
			"Image %s is missing the %s label (no %q capability); with metadata mode block or assign on Docker/rootful Podman, the broker will rely on the unlabeled-image fallback and refuse to start if sciontool lacks netns-setup",
			img, scionruntime.ImageSciontoolCapabilitiesLabel, scionruntime.SciontoolCapNetnsSetup,
		)
	} else {
		msg = fmt.Sprintf(
			"Image %s label %s=%q lacks %q; with metadata mode block or assign on Docker/rootful Podman, the broker will rely on the unlabeled-image fallback and refuse to start if sciontool lacks netns-setup",
			img, scionruntime.ImageSciontoolCapabilitiesLabel, rawCap, scionruntime.SciontoolCapNetnsSetup,
		)
	}
	return scionruntime.CheckResult{
		Name:        fmt.Sprintf("container-image-capabilities[%s]", img),
		Status:      "warn",
		Message:     msg,
		Remediation: "Rebuild or pull an agent image matching this scion binary",
	}, true
}

func checkDoctorImageVersionSkew(img string, labels map[string]string, localVersion, localCommit string) (scionruntime.CheckResult, bool) {
	lv := strings.TrimSpace(localVersion)
	lc := strings.TrimSpace(localCommit)
	if isDevDoctorVersion(lv) {
		return scionruntime.CheckResult{}, false
	}

	hasLocalVersion := lv != ""
	hasLocalCommit := isKnownDoctorCommit(lc)
	if !hasLocalVersion && !hasLocalCommit {
		return scionruntime.CheckResult{}, false
	}

	imgVersion := strings.TrimSpace(labels[ociImageVersionLabel])
	imgRevision := strings.TrimSpace(labels[ociImageRevisionLabel])
	if imgVersion == "" && imgRevision == "" {
		return scionruntime.CheckResult{}, false
	}

	var diffs []string
	if hasLocalVersion && imgVersion != "" && imgVersion != lv {
		diffs = append(diffs, fmt.Sprintf("version %s != local %s", imgVersion, lv))
	}
	if hasLocalCommit && imgRevision != "" && !doctorCommitsMatch(imgRevision, lc) {
		diffs = append(diffs, fmt.Sprintf("revision %s != local %s", shortDoctorCommit(imgRevision), shortDoctorCommit(lc)))
	}
	if len(diffs) == 0 {
		return scionruntime.CheckResult{}, false
	}

	return scionruntime.CheckResult{
		Name:    fmt.Sprintf("container-image-version[%s]", img),
		Status:  "info",
		Message: fmt.Sprintf("Image %s differs from local scion binary (%s)", img, strings.Join(diffs, "; ")),
	}, true
}

func isDevDoctorVersion(v string) bool {
	return strings.EqualFold(v, "dev") || strings.EqualFold(v, "unknown") || v == "(devel)"
}

func isKnownDoctorCommit(c string) bool {
	return c != "" && !strings.EqualFold(c, "unknown") && !strings.EqualFold(c, "none") && !strings.EqualFold(c, "dirty")
}

func doctorCommitsMatch(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == b {
		return true
	}
	if len(a) >= 7 && len(b) >= 7 {
		return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
	}
	return false
}

func shortDoctorCommit(c string) string {
	c = strings.TrimSpace(c)
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

func matchLocalHarnessImages(localImages, configuredImages []string, cli string) []string {
	if len(localImages) == 0 || len(configuredImages) == 0 {
		return nil
	}

	candidates := make(map[string]struct{}, len(configuredImages)*2)
	for _, ref := range configuredImages {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		candidates[ref] = struct{}{}
		hasTag := hasExplicitTagOrDigest(ref)
		if !hasTag {
			candidates[ref+":latest"] = struct{}{}
		}
		if cli == "podman" && config.IsShortFormImage(ref) {
			candidates["localhost/"+ref] = struct{}{}
			if !hasTag {
				candidates["localhost/"+ref+":latest"] = struct{}{}
			}
		}
	}

	matchedSet := make(map[string]struct{})
	for _, local := range localImages {
		local = strings.TrimSpace(local)
		if local == "" || local == "<none>:<none>" {
			continue
		}
		if _, ok := candidates[local]; ok {
			matchedSet[local] = struct{}{}
		}
	}

	matched := make([]string, 0, len(matchedSet))
	for img := range matchedSet {
		matched = append(matched, img)
	}
	sort.Strings(matched)
	return matched
}

func hasExplicitTagOrDigest(ref string) bool {
	if strings.Contains(ref, "@") {
		return true
	}
	lastSlash := strings.LastIndex(ref, "/")
	return strings.Contains(ref[lastSlash+1:], ":")
}
