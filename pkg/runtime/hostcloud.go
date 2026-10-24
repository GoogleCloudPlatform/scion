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
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// CloudHost is the result of local cloud-host detection.
type CloudHost struct {
	// Detected is true when a local signal identifies the host as a cloud VM.
	Detected bool
	// Source names the signal that matched: "dmi", "cloud-init" or
	// "guest-agent". Empty when nothing was detected.
	Source string
	// Provider is a short provider label (for example "gce" or "aws"), or
	// the cloud-init cloud name. Empty when nothing was detected.
	Provider string
}

// maxCloudInitBytes bounds how much of the cloud-init instance data is read.
// Real instance data is tens of KiB; a larger file is truncated, fails to
// parse, and counts as not detected.
const maxCloudInitBytes = 4 << 20

// DetectCloudHost reports whether the host is a cloud VM, using local files
// under root only (normally "/"); it makes no network requests. The signals,
// in order: a positive DMI vendor/product list, cloud-init instance data, and
// the GCE guest agent's files. On a non-Linux host, or when nothing matches
// or the files cannot be read, the host is reported as not detected.
//
// When the broker runs in a container, /sys/class/dmi is usually the host's
// while the cloud-init and guest-agent paths are the container's, so DMI is
// the main signal there.
func DetectCloudHost(root string) CloudHost {
	return detectCloudHost(root, goruntime.GOOS)
}

func detectCloudHost(root, goos string) CloudHost {
	if goos != "linux" {
		return CloudHost{}
	}
	if root == "" {
		root = "/"
	}
	if p := dmiCloudProvider(root); p != "" {
		return CloudHost{Detected: true, Source: "dmi", Provider: p}
	}
	if p := cloudInitCloudName(root); p != "" {
		return CloudHost{Detected: true, Source: "cloud-init", Provider: p}
	}
	if gceGuestAgentPresent(root) {
		return CloudHost{Detected: true, Source: "guest-agent", Provider: "gce"}
	}
	return CloudHost{}
}

// azureChassisAssetTag is the chassis asset tag Azure sets on its VMs. It is
// what separates an Azure VM from a local Hyper-V guest, which reports the
// same vendor and product.
const azureChassisAssetTag = "7783-7084-3265-9085-8269-3286-77"

// dmiCloudProvider matches the DMI identity fields against a positive list
// of cloud providers and returns the provider label, or "".
func dmiCloudProvider(root string) string {
	dir := filepath.Join(root, "sys", "class", "dmi", "id")
	read := func(name string) string {
		b, err := readFileLimited(filepath.Join(dir, name), 4096)
		if err != nil {
			return ""
		}
		return strings.ToLower(strings.TrimSpace(string(b)))
	}
	sysVendor := read("sys_vendor")
	product := read("product_name")
	biosVendor := read("bios_vendor")
	biosVersion := read("bios_version")
	assetTag := read("chassis_asset_tag")

	switch {
	case product == "google compute engine":
		return "gce"
	case sysVendor == "amazon ec2", biosVendor == "amazon ec2",
		strings.HasPrefix(product, "hvm domu") && strings.Contains(biosVersion, "amazon"):
		return "aws"
	case sysVendor == "microsoft corporation" && product == "virtual machine" && assetTag == azureChassisAssetTag:
		return "azure"
	case assetTag == "oraclecloud.com":
		return "oracle"
	case sysVendor == "alibaba cloud", product == "alibaba cloud ecs":
		return "alibaba"
	case product == "openstack nova", product == "openstack compute", sysVendor == "openstack foundation":
		return "openstack"
	case sysVendor == "digitalocean":
		return "digitalocean"
	case sysVendor == "hetzner":
		return "hetzner"
	}
	return ""
}

// cloudInitClouds are the cloud-init cloud names that identify a provider on
// the DMI list. cloud-init derives the name from its datasource, and many
// datasources (vmware, ovf, maas, wsl, configdrive, nocloud, ...) are used on
// hosts without a cloud metadata service, so only these names count.
var cloudInitClouds = map[string]bool{
	"aws":          true,
	"gce":          true,
	"azure":        true,
	"oracle":       true,
	"aliyun":       true,
	"openstack":    true,
	"digitalocean": true,
	"hetzner":      true,
}

// cloudInitCloudName returns the cloud name recorded by cloud-init when it
// is on the cloudInitClouds list, or "" when the file is missing, unreadable
// or malformed, or the name is not on the list. The first non-empty name of
// v1.cloud_name, v1."cloud-name" and cloud_name decides.
func cloudInitCloudName(root string) string {
	b, err := readFileLimited(filepath.Join(root, "run", "cloud-init", "instance-data.json"), maxCloudInitBytes)
	if err != nil {
		return ""
	}
	var data struct {
		CloudName string `json:"cloud_name"`
		V1        struct {
			CloudName       string `json:"cloud_name"`
			CloudNameHyphen string `json:"cloud-name"`
		} `json:"v1"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		return ""
	}
	for _, name := range []string{data.V1.CloudName, data.V1.CloudNameHyphen, data.CloudName} {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if cloudInitClouds[name] {
			return name
		}
		return ""
	}
	return ""
}

// gceGuestAgentFiles are files the GCE guest agent installs.
var gceGuestAgentFiles = []string{
	"etc/default/instance_configs.cfg",
	"usr/bin/google_guest_agent",
	"lib/systemd/system/google-guest-agent.service",
	"usr/lib/systemd/system/google-guest-agent.service",
}

func gceGuestAgentPresent(root string) bool {
	for _, rel := range gceGuestAgentFiles {
		if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
			return true
		}
	}
	return false
}

func readFileLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, limit))
}

// HostNetworkBlockPolicyEnvVar is the broker setting that controls whether
// an agent with metadata block mode may start under host networking, where
// the block cannot be enforced:
//
//   - unset or "auto": refuse on a detected cloud host, otherwise start
//     with a warning;
//   - "strict": always refuse;
//   - "allow": start on a detected cloud host too, logging an explicit
//     acknowledgement at Warn on every such start.
//
// Any other value is treated as "strict".
const HostNetworkBlockPolicyEnvVar = "SCION_HOST_NETWORK_BLOCK_POLICY"

// HostNetworkBlockPolicy values.
const (
	HostNetworkBlockPolicyAuto   = "auto"
	HostNetworkBlockPolicyStrict = "strict"
	HostNetworkBlockPolicyAllow  = "allow"
)

// ParseHostNetworkBlockPolicy normalizes a policy value. Empty means auto.
// An unrecognized value returns strict and known=false, so the caller can
// log it; it never relaxes the policy.
func ParseHostNetworkBlockPolicy(v string) (policy string, known bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", HostNetworkBlockPolicyAuto:
		return HostNetworkBlockPolicyAuto, true
	case HostNetworkBlockPolicyStrict:
		return HostNetworkBlockPolicyStrict, true
	case HostNetworkBlockPolicyAllow:
		return HostNetworkBlockPolicyAllow, true
	}
	return HostNetworkBlockPolicyStrict, false
}

// HostNetworkBlockAction is the outcome for a block-mode start under host
// networking.
type HostNetworkBlockAction int

const (
	// HostNetworkBlockWarn starts the agent with the standard warning.
	HostNetworkBlockWarn HostNetworkBlockAction = iota
	// HostNetworkBlockRefuse refuses the start.
	HostNetworkBlockRefuse
	// HostNetworkBlockAllowAcknowledged starts on a detected cloud host
	// because the operator set the allow policy.
	HostNetworkBlockAllowAcknowledged
)

// DecideHostNetworkBlock maps a normalized policy and the detection result
// to an action.
func DecideHostNetworkBlock(policy string, cloud CloudHost) HostNetworkBlockAction {
	switch policy {
	case HostNetworkBlockPolicyStrict:
		return HostNetworkBlockRefuse
	case HostNetworkBlockPolicyAllow:
		if cloud.Detected {
			return HostNetworkBlockAllowAcknowledged
		}
		return HostNetworkBlockWarn
	case HostNetworkBlockPolicyAuto:
		if cloud.Detected {
			return HostNetworkBlockRefuse
		}
		return HostNetworkBlockWarn
	}
	return HostNetworkBlockRefuse
}
