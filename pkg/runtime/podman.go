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

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

const (
	// Podman added --add-host <host>:host-gateway support in 4.7.0.
	podmanMinHostGatewayMajor = 4
	podmanMinHostGatewayMinor = 7
	podmanNativeHostAlias     = "host.containers.internal"

	// Rootless Podman network backends that support host-loopback translation.
	podmanRootlessNetPasta       = "pasta"
	podmanRootlessNetSlirp4netns = "slirp4netns"

	// Podman 5's pasta maps host.containers.internal to 169.254.1.2, and
	// slirp4netns exposes the host loopback gateway at 10.0.2.2 when
	// allow_host_loopback=true is enabled.
	podmanPastaLoopbackAddr = "169.254.1.2"
	podmanSlirpLoopbackAddr = "10.0.2.2"
)

// ErrPodmanHostGatewayUnsupported is returned (wrapped in a *LaunchFailure)
// when a container requires --add-host <host>:host-gateway on a Podman version
// older than 4.7.
var ErrPodmanHostGatewayUnsupported = errors.New("podman does not support --add-host host-gateway; upgrade to Podman 4.7 or newer")

type PodmanRuntime struct {
	Command  string
	Host     string
	Rootless bool // true when Podman is running in rootless mode
	// SingleUID is true when rootless Podman has no subordinate UID/GID ranges
	// (only container UID/GID 0 is mapped to the host user). In this mode
	// --userns=keep-id:uid=1000,gid=1000 is rejected by Podman and exec must
	// target container UID 0 ("root"), which maps directly to the host user.
	SingleUID bool
	// RootlessNetwork identifies the detected rootless networking backend
	// ("pasta" on Podman 5+, "slirp4netns" on Podman 4.x or systems without
	// pasta, or "" when unknown/rootful).
	RootlessNetwork string

	hostGatewayOnce      sync.Once
	hostGatewaySupported bool
}

// NewPodmanRuntime creates a new PodmanRuntime after verifying that the podman
// binary exists and meets the minimum version requirement (4.x+).
// If cmdOverride is non-empty, it is used as the podman binary path instead of
// looking up "podman" on $PATH. Returns an ErrorRuntime if validation fails.
func NewPodmanRuntime(cmdOverride string) Runtime {
	command := "podman"
	if cmdOverride != "" {
		command = cmdOverride
	}

	if cmdOverride == "" {
		if _, err := exec.LookPath(command); err != nil {
			return &ErrorRuntime{Err: fmt.Errorf("podman not found on PATH: %w", err)}
		}
	}

	// Check version: minimum 4.x
	out, err := exec.Command(command, "--version").Output()
	if err != nil {
		return &ErrorRuntime{Err: fmt.Errorf("failed to get podman version: %w", err)}
	}

	versionOutput := strings.TrimSpace(string(out))
	version := parsePodmanVersion(versionOutput)
	major, err := parseMajorVersion(version)
	if err != nil {
		return &ErrorRuntime{Err: fmt.Errorf("failed to parse podman version %q: %w", version, err)}
	}
	if major < 4 {
		return &ErrorRuntime{Err: fmt.Errorf("podman version %s is below the minimum supported version 4.x", version)}
	}

	// Detect rootless mode, subordinate UID/GID availability, and rootless
	// network backend in a single podman info probe.
	hostInfo := probePodmanHostInfo(command, major)

	rt := &PodmanRuntime{
		Command:         command,
		Rootless:        hostInfo.Rootless,
		SingleUID:       hostInfo.SingleUID,
		RootlessNetwork: hostInfo.RootlessNetwork,
	}
	supported := probePodmanHostGateway(context.Background(), command, versionOutput)
	rt.hostGatewayOnce.Do(func() {
		rt.hostGatewaySupported = supported
	})
	return rt
}

type podmanVersionJSON struct {
	Client *struct {
		Version string `json:"Version"`
	} `json:"Client"`
	Server *struct {
		Version string `json:"Version"`
	} `json:"Server"`
}

// parsePodmanVersionJSON extracts major.minor from "podman version --format json"
// output. When both Server and Client are present (remote Podman / podman machine),
// the Server version is preferred because the service enforces --add-host support.
// When no separate Server is present (local daemonless Podman), it falls back to
// Client.Version.
func parsePodmanVersionJSON(output string) (major, minor int, ok bool) {
	trimmed := strings.TrimSpace(output)
	idx := strings.IndexByte(trimmed, '{')
	if idx < 0 {
		return 0, 0, false
	}
	var parsed podmanVersionJSON
	if err := json.Unmarshal([]byte(trimmed[idx:]), &parsed); err != nil {
		return 0, 0, false
	}
	if parsed.Server != nil && strings.TrimSpace(parsed.Server.Version) != "" {
		if maj, min, ok := parsePodmanMajorMinorVersion(parsed.Server.Version); ok {
			return maj, min, true
		}
	}
	if parsed.Client != nil && strings.TrimSpace(parsed.Client.Version) != "" {
		if maj, min, ok := parsePodmanMajorMinorVersion(parsed.Client.Version); ok {
			return maj, min, true
		}
	}
	return 0, 0, false
}

// PodmanSupportsHostGateway reports whether Podman supports the special
// "host-gateway" address used by --add-host. Support was added in Podman 4.7;
// older releases (e.g. Podman 4.3.1 on Debian 12) reject it with
// "invalid IP address in add-host: host-gateway". It prefers the Podman server
// version (for remote Podman / podman machine) via "podman version --format json",
// falling back to the client version when there is no separate server. On any
// probe failure we conservatively assume support is present, matching
// DockerSupportsHostGateway.
func PodmanSupportsHostGateway(ctx context.Context, command string) bool {
	return probePodmanHostGateway(ctx, command, "")
}

const podmanHostGatewayProbeTimeout = 5 * time.Second

func probePodmanHostGateway(ctx context.Context, command, clientVersionFallback string) bool {
	return probePodmanHostGatewayWithTimeout(ctx, command, clientVersionFallback, podmanHostGatewayProbeTimeout)
}

func probePodmanHostGatewayWithTimeout(ctx context.Context, command, clientVersionFallback string, timeout time.Duration) bool {
	if command == "" {
		command = "podman"
	}
	jsonCtx, jsonCancel := context.WithTimeout(ctx, timeout)
	out, err := runSimpleCommand(jsonCtx, command, "version", "--format", "json")
	jsonCancel()
	if err == nil {
		if major, minor, ok := parsePodmanVersionJSON(out); ok {
			return podmanVersionSupportsHostGateway(major, minor)
		}
		runtimeLog().Debug("Unable to parse Podman JSON version for host-gateway support", "output", strings.TrimSpace(out))
	} else {
		runtimeLog().Debug("Unable to probe Podman JSON version for host-gateway support", "error", err)
	}

	if clientVersionFallback == "" {
		fallbackCtx, fallbackCancel := context.WithTimeout(ctx, timeout)
		fallbackOut, fallbackErr := runSimpleCommand(fallbackCtx, command, "--version")
		fallbackCancel()
		if fallbackErr != nil {
			runtimeLog().Debug("Unable to probe Podman client version for host-gateway support", "error", fallbackErr)
			return true
		}
		clientVersionFallback = strings.TrimSpace(fallbackOut)
	}
	major, minor, ok := parsePodmanMajorMinorVersion(clientVersionFallback)
	if !ok {
		runtimeLog().Debug("Unable to parse Podman version for host-gateway support", "version", clientVersionFallback)
		return true
	}
	return podmanVersionSupportsHostGateway(major, minor)
}

func podmanVersionSupportsHostGateway(major, minor int) bool {
	return major > podmanMinHostGatewayMajor || (major == podmanMinHostGatewayMajor && minor >= podmanMinHostGatewayMinor)
}

// parsePodmanMajorMinorVersion parses the leading "major.minor" from
// "podman --version" output (e.g. "podman version 4.7.0" or "4.3.1"). It
// tolerates a leading "v"/"V" prefix, pre-release suffixes (e.g. "4.7-dev"),
// and scans line-by-line so warning noise does not defeat the probe.
func parsePodmanMajorMinorVersion(output string) (major, minor int, ok bool) {
	for _, line := range strings.Split(output, "\n") {
		ver := strings.TrimLeft(strings.TrimSpace(parsePodmanVersion(strings.TrimSpace(line))), "vV")
		parts := strings.SplitN(ver, ".", 3)
		if len(parts) < 2 {
			continue
		}
		maj, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		minorStr := parts[1]
		if idx := strings.IndexAny(minorStr, "-+~"); idx >= 0 {
			minorStr = minorStr[:idx]
		}
		min, err := strconv.Atoi(minorStr)
		if err != nil {
			continue
		}
		return maj, min, true
	}
	return 0, 0, false
}

func (r *PodmanRuntime) supportsHostGateway(ctx context.Context) bool {
	r.hostGatewayOnce.Do(func() {
		r.hostGatewaySupported = PodmanSupportsHostGateway(ctx, r.Command)
	})
	return r.hostGatewaySupported
}

// filterPodmanExtraHosts checks config.ExtraHosts for "host-gateway" targets
// and guards against Podman < 4.7 rejecting them. When Podman lacks support:
//   - host.containers.internal:host-gateway is omitted with a warning, since
//     Podman populates host.containers.internal natively without --add-host
//     (and rootless loopback routing is configured separately via
//     applyPodmanRootlessLoopback when needed).
//   - Any other <host>:host-gateway entry fails with a typed *LaunchFailure
//     naming the minimum Podman version (4.7).
func filterPodmanExtraHosts(ctx context.Context, extraHosts []string, supports func(context.Context) bool) ([]string, error) {
	hasHostGateway := false
	for _, h := range extraHosts {
		if _, target, ok := strings.Cut(h, ":"); ok && strings.EqualFold(target, "host-gateway") {
			hasHostGateway = true
			break
		}
	}
	if !hasHostGateway || supports(ctx) {
		return extraHosts, nil
	}

	filtered := make([]string, 0, len(extraHosts))
	for _, h := range extraHosts {
		name, target, ok := strings.Cut(h, ":")
		if !ok || !strings.EqualFold(target, "host-gateway") {
			filtered = append(filtered, h)
			continue
		}
		if strings.EqualFold(name, podmanNativeHostAlias) {
			runtimeLog().Warn("Omitting --add-host host.containers.internal:host-gateway: Podman < 4.7 does not support host-gateway (Podman defines host.containers.internal natively; rootless loopback access is configured via the network backend)")
			continue
		}
		return nil, newLaunchFailure(
			api.LaunchCodePodmanHostGatewayUnsupported,
			api.LaunchStepLaunching,
			"",
			fmt.Errorf("%w: cannot pass --add-host %s", ErrPodmanHostGatewayUnsupported, h),
		)
	}
	return filtered, nil
}

// parsePodmanVersion extracts the version string from "podman version X.Y.Z" output.
func parsePodmanVersion(output string) string {
	// Output format: "podman version X.Y.Z"
	parts := strings.Fields(output)
	if len(parts) >= 3 {
		return parts[len(parts)-1]
	}
	return output
}

// parseMajorVersion extracts the major version number from a semver string.
func parseMajorVersion(version string) (int, error) {
	parts := strings.Split(version, ".")
	if len(parts) < 1 {
		return 0, fmt.Errorf("unexpected version format: %s", version)
	}
	return strconv.Atoi(parts[0])
}

type podmanIDMapEntry struct {
	ContainerID int `json:"container_id"`
	HostID      int `json:"host_id"`
	Size        int `json:"size"`
}

type podmanInfoJSON struct {
	Version *struct {
		Version string `json:"Version"`
	} `json:"version"`
	Host *struct {
		Security *struct {
			Rootless bool `json:"rootless"`
		} `json:"security"`
		IDMappings *struct {
			UIDMap []podmanIDMapEntry `json:"uidmap"`
			GIDMap []podmanIDMapEntry `json:"gidmap"`
		} `json:"idMappings"`
		Pasta *struct {
			Executable string `json:"executable"`
		} `json:"pasta"`
		Slirp4netns *struct {
			Executable string `json:"executable"`
		} `json:"slirp4netns"`
	} `json:"host"`
}

type podmanHostInfo struct {
	Rootless        bool
	SingleUID       bool
	RootlessNetwork string
}

func hasSingleIDMapping(entries []podmanIDMapEntry) bool {
	if len(entries) == 0 {
		return false
	}
	total := 0
	for _, e := range entries {
		if e.Size > 0 {
			total += e.Size
		}
	}
	return total <= 1
}

// parsePodmanInfoJSON parses the output of "podman info --format json" (and
// tolerates plain "true"/"false" output from legacy mocks). fallbackMajor is
// used when the info payload omits version.Version.
func parsePodmanInfoJSON(output string, fallbackMajor int) podmanHostInfo {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return podmanHostInfo{}
	}
	idx := strings.IndexByte(trimmed, '{')
	if idx < 0 {
		return podmanHostInfo{Rootless: trimmed == "true"}
	}
	var parsed podmanInfoJSON
	if err := json.Unmarshal([]byte(trimmed[idx:]), &parsed); err != nil {
		util.Debugf("podman: failed to parse info JSON: %v", err)
		return podmanHostInfo{}
	}
	if parsed.Host == nil || parsed.Host.Security == nil || !parsed.Host.Security.Rootless {
		return podmanHostInfo{}
	}

	info := podmanHostInfo{Rootless: true}
	if parsed.Host.IDMappings != nil {
		if hasSingleIDMapping(parsed.Host.IDMappings.UIDMap) || hasSingleIDMapping(parsed.Host.IDMappings.GIDMap) {
			info.SingleUID = true
		}
	}

	effectiveMajor := fallbackMajor
	if parsed.Version != nil && strings.TrimSpace(parsed.Version.Version) != "" {
		if maj, _, ok := parsePodmanMajorMinorVersion(parsed.Version.Version); ok {
			effectiveMajor = maj
		}
	}

	hasPasta := parsed.Host.Pasta != nil && strings.TrimSpace(parsed.Host.Pasta.Executable) != ""
	hasSlirp := parsed.Host.Slirp4netns != nil && strings.TrimSpace(parsed.Host.Slirp4netns.Executable) != ""
	switch {
	case effectiveMajor >= 5 && hasPasta:
		info.RootlessNetwork = podmanRootlessNetPasta
	case hasSlirp:
		info.RootlessNetwork = podmanRootlessNetSlirp4netns
	case hasPasta:
		info.RootlessNetwork = podmanRootlessNetPasta
	}
	return info
}

// probePodmanHostInfo queries "podman info --format json" (with a bounded
// timeout matching probePodmanHostGateway) to detect rootless mode, single-UID
// namespace constraints (no subordinate UID/GID allocation), and the available
// rootless network backend.
func probePodmanHostInfo(command string, fallbackMajor int) podmanHostInfo {
	ctx, cancel := context.WithTimeout(context.Background(), podmanHostGatewayProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, command, "info", "--format", "json").Output()
	if err != nil {
		util.Debugf("podman: failed to probe host info: %v", err)
		return podmanHostInfo{}
	}
	info := parsePodmanInfoJSON(string(out), fallbackMajor)
	util.Debugf("podman: rootless=%v single_uid=%v rootless_network=%q", info.Rootless, info.SingleUID, info.RootlessNetwork)
	return info
}

func urlOrHostMatches(raw, targetHost string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	if u, err := url.Parse(trimmed); err == nil && u.Hostname() != "" {
		return strings.EqualFold(u.Hostname(), targetHost)
	}
	if h, _, err := net.SplitHostPort(trimmed); err == nil {
		return strings.EqualFold(h, targetHost)
	}
	return strings.EqualFold(trimmed, targetHost)
}

// needsPodmanRootlessLoopback reports whether a rootless Podman container
// requires host loopback translation for host.containers.internal.
//
// In broker mode, only operator-controlled fields (TrustedHubEndpoint and
// ExtraHosts) are consulted so untrusted template/creator environment entries
// cannot widen host loopback reachability on a shared broker. Outside broker
// mode (local workstation runs), config.Env and config.ResolvedAuth.EnvVars
// URLs targeting host.containers.internal are also inspected.
func needsPodmanRootlessLoopback(config RunConfig) bool {
	if urlOrHostMatches(config.TrustedHubEndpoint, podmanNativeHostAlias) {
		return true
	}
	for _, h := range config.ExtraHosts {
		if name, _, ok := strings.Cut(h, ":"); ok && strings.EqualFold(strings.TrimSpace(name), podmanNativeHostAlias) {
			return true
		}
	}
	if !config.BrokerMode {
		for _, entry := range config.Env {
			_, val, ok := strings.Cut(entry, "=")
			if ok && urlOrHostMatches(val, podmanNativeHostAlias) {
				return true
			}
		}
		if config.ResolvedAuth != nil {
			for _, val := range config.ResolvedAuth.EnvVars {
				if urlOrHostMatches(val, podmanNativeHostAlias) {
					return true
				}
			}
		}
	}
	return false
}

// ensurePodmanHostAliasExtraHost returns a copy of extraHosts with
// host.containers.internal pinned to loopbackAddr. It replaces any
// host-gateway placeholder (which resolves to the host's non-loopback NIC
// under rootless pasta/slirp4netns) while preserving any explicit
// operator-provided IP mapping without mutating the caller's slice.
func ensurePodmanHostAliasExtraHost(extraHosts []string, loopbackAddr string) []string {
	replaced := false
	hasExplicitIP := false
	res := make([]string, len(extraHosts), len(extraHosts)+1)
	copy(res, extraHosts)
	for i, h := range res {
		name, target, ok := strings.Cut(h, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), podmanNativeHostAlias) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(target), "host-gateway") {
			res[i] = podmanNativeHostAlias + ":" + loopbackAddr
			replaced = true
		} else {
			hasExplicitIP = true
		}
	}
	if !replaced && !hasExplicitIP {
		res = append(res, podmanNativeHostAlias+":"+loopbackAddr)
	}
	return res
}

// applyPodmanRootlessLoopback enables host-loopback translation on rootless
// Podman when the container targets host.containers.internal and no explicit
// NetworkMode was configured.
//
// Under Podman 5's default pasta backend, --map-host-loopback,169.254.1.2 maps
// 169.254.1.2 to host 127.0.0.1 and host.containers.internal is pinned to
// 169.254.1.2 so resolution is consistent across Podman 5.x releases. Under
// slirp4netns, allow_host_loopback=true exposes host loopback at 10.0.2.2 and
// host.containers.internal is pinned to 10.0.2.2.
func applyPodmanRootlessLoopback(config *RunConfig, rootless bool, rootlessNetwork string, wantLoopback bool) {
	if !rootless || !wantLoopback || config.NetworkMode != "" {
		return
	}
	switch rootlessNetwork {
	case podmanRootlessNetPasta:
		config.NetworkMode = "pasta:--map-host-loopback," + podmanPastaLoopbackAddr
		config.ExtraHosts = ensurePodmanHostAliasExtraHost(config.ExtraHosts, podmanPastaLoopbackAddr)
	case podmanRootlessNetSlirp4netns:
		config.NetworkMode = "slirp4netns:allow_host_loopback=true"
		config.ExtraHosts = ensurePodmanHostAliasExtraHost(config.ExtraHosts, podmanSlirpLoopbackAddr)
	default:
		runtimeLog().Warn("Rootless loopback routing requested for host.containers.internal, but no supported rootless network backend (pasta or slirp4netns) was detected",
			"rootless_network", rootlessNetwork)
	}
}

func (r *PodmanRuntime) Name() string {
	return "podman"
}

// ExecUser implements Runtime. Returns "scion" when sciontool init drops
// privileges to the scion user (UID 1000) — both in rootful mode and in
// rootless mode with subordinate UID/GID mappings (--userns=keep-id).
//
// In single-UID rootless mode (no /etc/subuid or /etc/subgid range), container
// UID 0 is the only mapped UID (mapped 1:1 to the unprivileged host user), so
// sciontool init skips remapping and runs tmux as UID 0 (/tmp/tmux-0/). In
// that case ExecUser returns "root" so podman exec targets the mapped UID and
// matches the tmux socket owner.
func (r *PodmanRuntime) ExecUser() string {
	if r.Rootless && r.SingleUID {
		return "root"
	}
	return "scion"
}

func (r *PodmanRuntime) Run(ctx context.Context, config RunConfig) (string, error) {
	// N1-5: Podman rootless + NFS is unsupported. keep-id subuid ranges
	// yield no stable on-wire UID, so files on the shared NFS export would
	// have unpredictable ownership across nodes. Reject early with a clear
	// error (design §9.1).
	if r.Rootless && config.WorkspaceBackendName == "nfs" {
		return "", fmt.Errorf("podman rootless with NFS workspace backend is not supported: " +
			"keep-id subuid ranges cannot produce a stable on-wire UID for shared NFS storage; " +
			"use rootful Docker or Podman for NFS-backed projects")
	}

	// Rootless Podman runs PID 1 as the keep-id user (SCION_KEEPID_UID below),
	// which drops privileges before the metadata server starts, so the
	// in-container interception cannot be installed. Do not grant NET_ADMIN
	// or mark the interception required there; the metadata env vars remain
	// the mechanism.
	if r.Rootless {
		config.MetadataInterception = false
	}
	if err := checkFirewallWithoutMetadataInterception(config); err != nil {
		return "", err
	}

	if err := prepareContainerSecretEnv(&config); err != nil {
		return "", err
	}
	config.RuntimeName = r.Name()

	// Capture host.containers.internal loopback intent before
	// filterPodmanExtraHosts may drop host.containers.internal:host-gateway on
	// Podman < 4.7.
	wantLoopback := r.Rootless && config.NetworkMode == "" && needsPodmanRootlessLoopback(config)

	extraHosts, err := filterPodmanExtraHosts(ctx, config.ExtraHosts, r.supportsHostGateway)
	if err != nil {
		return "", err
	}
	config.ExtraHosts = extraHosts

	applyPodmanRootlessLoopback(&config, r.Rootless, r.RootlessNetwork, wantLoopback)

	args, err := buildCommonRunArgs(config)
	if err != nil {
		return "", err
	}

	// sciontool already handles PID 1 responsibilities (zombie reaping, signal forwarding),
	// so we don't use --init to avoid competing init processes.
	newArgs := []string{"run", "-t"}

	// In rootless mode with subordinate UID/GID ranges, map the host user's
	// UID into the container as UID 1000 (the scion user created in the
	// Dockerfile). This ensures:
	//  1. Bind-mounted files have correct host-user ownership on both sides.
	//  2. The process runs as the scion user (UID 1000) inside the container,
	//     so harness config in /home/scion is accessible.
	// Without the uid/gid mapping, --userns=keep-id would only work when the
	// host user happens to be UID 1000. The explicit mapping handles any host
	// UID (e.g., 501 on macOS).
	//
	// SCION_KEEPID_UID tells sciontool init that the host user is mapped to
	// this container UID via keep-id, so it should drop privileges to that
	// UID early instead of trying to remap the scion user via usermod.
	// This env var is necessary because /proc/self/uid_map inside the
	// container only shows the mapping to the immediate parent namespace
	// (Podman's namespace), not the host — so init cannot derive this from
	// the uid_map alone.
	//
	// When SingleUID is true (no subordinate UID/GID allocation), UID 1000 is
	// unmappable and Podman rejects --userns=keep-id:uid=1000,gid=1000.
	// Omitting keep-id maps the host user 1:1 to container UID 0, and
	// sciontool init's unmapped-UID fallback handles /home/scion cleanly.
	if r.Rootless && !r.SingleUID {
		newArgs = append(newArgs, "--userns=keep-id:uid=1000,gid=1000")
		newArgs = append(newArgs, "-e", "SCION_KEEPID_UID=1000")
	} else if r.Rootless && r.SingleUID {
		util.Debugf("podman: single-UID rootless namespace detected; omitting --userns=keep-id and running as mapped container UID 0")
	}

	// Apply resource constraints from config.
	//
	// TODO(cgroup-limits): when r.Rootless is true and the host is on cgroup v1,
	// Podman cannot apply resource limits and rejects these flags, so the agent
	// fails to start instead of falling back to an unlimited container. This is
	// now reachable by default because config.BuiltinDefaultResources() supplies
	// limits.cpu "2" for every agent. probePodmanHostInfo already gives us half
	// the signal; the fix is to also probe
	// `podman info --format '{{.Host.CgroupsVersion}}'` at construction and skip
	// these flags with a warning when limits are unsupported. Not implemented
	// here — affected deployments must set
	// `runtime.enforce_resource_defaults: false` until it lands.
	newArgs, err = appendContainerResourceArgs(newArgs, config.Resources)
	if err != nil {
		return "", err
	}

	newArgs = appendSharedDirGroupArgs(newArgs, config, podmanRuntimeName(r.Rootless), !r.Rootless)

	newArgs = append(newArgs, args[1:]...)

	// Secret values go to podman through its environment, not argv
	// (moveSecretEnvOutOfArgv).
	newArgs, secretEnv, err := moveSecretEnvOutOfArgv(newArgs, config.Image, podmanSecretEnvNames(config), os.LookupEnv)
	if err != nil {
		return "", err
	}

	WriteRuntimeDebugFile(config, r.Command, newArgs)

	// Async-launch gate immediately before the container create (design
	// t1-async-create-v11.md §3.8.3); a no-op on the synchronous path.
	hooks := config.launchHooks()
	if err := hooks.checkpoint(ctx, CheckpointStepLaunching); err != nil {
		return "", err
	}
	if config.MetadataInterception {
		checkImageNetnsSetupSupport(ctx, r.Command, config.Image)
	}
	// Rootless podman that starts its user namespace's pause process
	// (catatonit -P) hands it its own environment, and the pause process
	// outlives the run: the secret values would stay in its
	// /proc/<pid>/environ. An env-free "podman unshare true" first starts
	// the pause process if none runs, and "run" then joins it. A remote
	// client has no "unshare", and its pause process belongs to the podman
	// service, not to this environment, so a failure is logged at debug
	// level only.
	if r.Rootless && len(secretEnv) > 0 {
		if uout, uerr := runSimpleCommand(ctx, r.Command, "unshare", "true"); uerr != nil && ctx.Err() == nil {
			runtimeLog().Debug("podman unshare before a launch with secret env failed; a pause process this run starts keeps podman's environment",
				"error", uerr, "output", strings.TrimSpace(uout))
		}
	}
	out, err := runSimpleCommandWithEnv(ctx, secretEnv, r.Command, newArgs...)
	if err != nil {
		if ctx.Err() != nil {
			// The caller gave up while the daemon may still have been
			// creating/starting the container. Clean up any partial result
			// instead of leaking it. See ptone/scion#1886.
			rollbackCancelledCreate(r.Command, config.Name, config.Labels[api.LabelRunID])
			return "", ctx.Err()
		}
		runErr := fmt.Errorf("container run failed: %w (output: %s)", err, out)
		if code, step := classifyPodmanRunError(out, err); code != "" {
			if step == api.LaunchStepImagePull {
				hooks.progress(api.LaunchStepImagePull)
			}
			return "", newLaunchFailure(code, step, "", runErr)
		}
		return "", runErr
	}

	// "podman run -d" prints the full container ID, and only that, on
	// stdout (runSimpleCommand keeps stderr out of it). Anything else is
	// not an ID callers may address the container by: refuse it, and
	// remove what this run may have created, as for a cancelled create.
	id := strings.TrimSpace(out)
	if !podmanContainerIDPattern.MatchString(id) {
		err := fmt.Errorf("container run printed no container ID: got %q, want 64 lowercase hex characters", truncateForError(id))
		// The run exited 0, so a container almost certainly exists: say so
		// when it could not be removed.
		if rbErr := rollbackCreate(r.Command, config.Name, config.Labels[api.LabelRunID]); rbErr != nil {
			return "", fmt.Errorf("%w; a container from this run may still exist: %v", err, rbErr)
		}
		return "", err
	}
	if config.MetadataInterception {
		if err := runNetnsSetup(ctx, r.Command, id, config); err != nil {
			removeNetnsSetupContainers(ctx, r.Command, id)
			if rmErr := removeCreatedContainer(r.Command, id); rmErr != nil {
				return "", fmt.Errorf("metadata network setup failed: %w (container cleanup also failed: %v)", err, rmErr)
			}
			return "", fmt.Errorf("metadata network setup failed: %w", err)
		}
	}
	reportContainerCreated(hooks, config.Name, id)
	return id, nil
}

// podmanContainerIDPattern matches the full container ID "podman run -d"
// prints: 64 lowercase hex characters.
var podmanContainerIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Stop stops the container ref.ID. The engine container ID is already
// unique per run, so ref.RunID needs no further check here.
func (r *PodmanRuntime) Stop(ctx context.Context, ref RunRef) error {
	args := append([]string{"stop"}, stopTimeoutArgs(ref)...)
	out, err := runSimpleCommand(ctx, r.Command, append(args, ref.ID)...)
	if err != nil && out != "" {
		// Include podman's stderr output in the error so callers can match
		// on messages like "not running" (which runSimpleCommand's error
		// wrapping would otherwise discard).
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	return err
}

// Delete removes the container ref.ID and best-effort removes any leftover
// netns-setup containers labelled for it. The engine container ID is already
// unique per run, so ref.RunID needs no further check here.
func (r *PodmanRuntime) Delete(ctx context.Context, ref RunRef) error {
	removeNetnsSetupContainers(ctx, r.Command, ref.ID)
	_, err := runSimpleCommand(ctx, r.Command, "rm", "-f", ref.ID)
	return err
}

// podmanListOutput represents the JSON structure returned by "podman ps --format json".
// Unlike Docker which returns newline-separated JSON objects with string fields,
// Podman returns a JSON array with different field names and types:
//   - Id (not ID)
//   - Names is an array (not a single string)
//   - Labels is a map (not CSV)
type podmanListOutput struct {
	Id     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Status string            `json:"Status"`
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
}

func (r *PodmanRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	args := []string{"ps", "-a", "--no-trunc", "--format", "json"}
	cmd := exec.CommandContext(ctx, r.Command, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("podman ps failed: %w", err)
	}

	trimmed := strings.TrimSpace(string(out))

	// Podman returns a JSON array (possibly empty or "null"/"[]")
	var containers []podmanListOutput
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(trimmed), &containers); err != nil {
		return nil, fmt.Errorf("failed to parse podman ps output: %w", err)
	}

	var agents []api.AgentInfo
	for _, c := range containers {
		labels := c.Labels
		if labels == nil {
			labels = make(map[string]string)
		}

		// Filter by labels if requested
		match := LabelsMatchFilter(labels, labelFilter)

		if match {
			// Prefer the scion.name label (slugified) over Podman container name,
			// consistent with the Docker runtime. This ensures project-scoped
			// container names don't leak into display/lookup paths.
			name := labels["scion.name"]
			if name == "" && len(c.Names) > 0 {
				name = c.Names[0]
			}

			info := api.AgentInfo{
				ContainerID:     c.Id,
				RunID:           labels[api.LabelRunID],
				Name:            name,
				ContainerStatus: c.Status,
				Phase:           phaseFromContainerStatus(c.Status),
				Image:           c.Image,
				Labels:          labels,
				Annotations:     labels,
				Template:        labels["scion.template"],
				HarnessConfig:   labels["scion.harness_config"],
				HarnessAuth:     labels["scion.harness_auth"],
				Project:         projectkeys.ProjectNameFromLabels(labels),
				ProjectID:       projectkeys.ProjectIDFromLabels(labels),
				ProjectPath:     projectkeys.ProjectPathFromLabels(labels),
				Runtime:         r.Name(),
			}
			if code, ok := ExitCodeFromContainerStatus(c.Status); ok {
				ec := code
				info.ExitCode = &ec
				if code != 0 {
					info.ExitReason = string(state.ExitReasonCrashed)
				}
			}
			agents = append(agents, info)
		}
	}

	return agents, nil
}

func (r *PodmanRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	return runCombinedCommand(ctx, r.Command, "logs", id)
}

// execUserArgs returns the podman exec flags to target the container's runtime
// user. In single-UID rootless mode, container UID 0 ("root") maps to the
// unprivileged host user while sciontool's supervisor runs the harness with
// HOME=/home/scion; passing -e HOME=/home/scion prevents podman exec --user
// root from defaulting HOME to /root and diverging from the harness home.
func (r *PodmanRuntime) execUserArgs() []string {
	if r.Rootless && r.SingleUID {
		return []string{"-e", "HOME=/home/scion", "--user", r.ExecUser()}
	}
	return []string{"--user", r.ExecUser()}
}

func (r *PodmanRuntime) Attach(ctx context.Context, id string) error {
	// We need to find the container first to handle names properly
	agents, err := r.List(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	agent := findContainerAgent(agents, id)
	if agent == nil {
		return fmt.Errorf("agent '%s' container not found, it may have exited and been removed", id)
	}

	// Check if running
	if agent.Phase != string(state.PhaseRunning) {
		return fmt.Errorf("agent '%s' is not running (status: %s), use 'scion resume %s' to resume it", id, agent.ContainerStatus, id)
	}

	// Ensure tmux uses the latest client's terminal size so the session
	// redraws correctly on attach (handles containers started before the
	// window-size option was added to session creation).
	resizeArgs := append([]string{"exec"}, r.execUserArgs()...)
	resizeArgs = append(resizeArgs, agent.ContainerID, "tmux", "set-option", "-g", "window-size", "latest")
	_, _ = runSimpleCommand(ctx, r.Command, resizeArgs...)

	args := append([]string{"exec", "-it"}, ExecDetachKeysArgs(r.Command)...)
	args = append(args, r.execUserArgs()...)
	return runInteractiveCommand(r.Command, append(args, agent.ContainerID, "tmux", "attach", "-t", "scion")...)
}

func (r *PodmanRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	out, err := runSimpleCommand(ctx, r.Command, "image", "inspect", image)
	if err == nil {
		return true, nil
	}
	// Exit-code errors could mean "image not found" OR a daemon-level failure
	// (e.g. daemon unreachable). Both produce exec.ExitError with a non-zero
	// exit code, so we inspect the command output to distinguish the two.
	// Podman prints "image not known" when the image genuinely does not exist.
	if isExitError(err) {
		if isImageNotFoundOutput(out) {
			return false, nil
		}
		if ctx.Err() == nil && isPodmanDaemonConnectError(out) {
			return false, newLaunchFailure(api.LaunchCodeRuntimeUnavailable, api.LaunchStepLaunching, "", err)
		}
		return false, err
	}
	if ctx.Err() == nil && isRuntimeBinaryNotFound(err) {
		return false, newLaunchFailure(api.LaunchCodeRuntimeUnavailable, api.LaunchStepLaunching, "", err)
	}
	return false, err
}

func (r *PodmanRuntime) ImageID(ctx context.Context, image string) (string, error) {
	out, err := runSimpleCommand(ctx, r.Command, "image", "inspect", "--format", "{{.ID}}", image)
	if err != nil {
		return "", fmt.Errorf("image inspect failed: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func (r *PodmanRuntime) RemoveImage(ctx context.Context, image string) error {
	_, err := runSimpleCommand(ctx, r.Command, "rmi", image)
	return err
}

func (r *PodmanRuntime) PullImage(ctx context.Context, image string) error {
	reportLaunchProgress(ctx, api.LaunchStepImagePull)
	out, err := runSimpleCommand(ctx, r.Command, "pull", image)
	if err != nil {
		var pullErr error
		if trimmed := strings.TrimSpace(out); trimmed != "" {
			pullErr = fmt.Errorf("pull %q: %w\n%s", image, err, trimmed)
		} else {
			pullErr = fmt.Errorf("pull %q: %w", image, err)
		}
		if ctx.Err() == nil {
			if code := classifyPodmanPullError(image, out, err); code != "" {
				return newLaunchFailure(code, api.LaunchStepImagePull, "", pullErr)
			}
		}
		return pullErr
	}
	return nil
}

func (r *PodmanRuntime) Sync(ctx context.Context, id string, direction SyncDirection) error {
	agents, err := r.List(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	agent := findContainerAgent(agents, id)
	if agent == nil {
		return fmt.Errorf("agent '%s' container not found", id)
	}

	// Check for GCS volumes
	if encoded := agent.Labels["scion.gcs_volumes"]; encoded != "" {
		return syncGCSVolumes(ctx, encoded, direction)
	}

	// Podman runtime uses bind mounts for normal volumes, so sync is automatic/noop
	return nil
}

func (r *PodmanRuntime) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	// Resolve slug/name to actual container ID (container names include the
	// project prefix, e.g. "myproject--agent", so the bare slug won't match).
	if !resolvedContainerID(ctx) {
		if agents, err := r.List(ctx, nil); err == nil {
			id = resolveContainerID(agents, id)
		}
	}
	args := append([]string{"exec"}, r.execUserArgs()...)
	args = append(args, id)
	args = append(args, cmd...)
	out, err := runCombinedCommand(ctx, r.Command, args...)
	// A container removed after the lookup above must surface as
	// ErrContainerNotFound, not as the command's exit (ptone/scion#3655).
	return out, podmanExecNotFound.classifyExecErr(ctx, err, out, id, r.List)
}

// ExecWithStdin runs cmd inside the container with stdin piped from the
// given reader. The -i flag is required for `podman exec` to attach stdin.
// See #1355.
func (r *PodmanRuntime) ExecWithStdin(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
	if !resolvedContainerID(ctx) {
		if agents, err := r.List(ctx, nil); err == nil {
			id = resolveContainerID(agents, id)
		}
	}
	args := append([]string{"exec", "-i"}, r.execUserArgs()...)
	args = append(args, id)
	args = append(args, cmd...)
	return runSimpleCommandWithStdin(ctx, stdin, r.Command, args...)
}

// GetWorkspacePath returns the host path to the container's /workspace mount.
func (r *PodmanRuntime) GetWorkspacePath(ctx context.Context, id string) (string, error) {
	// Use podman inspect to get mount information
	out, err := runSimpleCommand(ctx, r.Command, "inspect", "--format", "{{json .Mounts}}", id)
	if err != nil {
		return "", fmt.Errorf("failed to inspect container: %w", err)
	}

	type mountInfo struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		Type        string `json:"Type"`
	}

	var mounts []mountInfo
	if err := json.Unmarshal([]byte(out), &mounts); err != nil {
		return "", fmt.Errorf("failed to parse mounts: %w", err)
	}

	// Look for /workspace mount
	for _, m := range mounts {
		if m.Destination == "/workspace" {
			return m.Source, nil
		}
	}

	return "", fmt.Errorf("no /workspace mount found for container %s", id)
}

// podmanRuntimeName names the Podman mode in shared-dir group warnings.
func podmanRuntimeName(rootless bool) string {
	if rootless {
		return "podman (rootless)"
	}
	return "podman"
}
