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
	"os"
	"path/filepath"
	"testing"
)

// writeFakeRoot creates files (relative path -> content) under a temp root.
func writeFakeRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func dmi(fields map[string]string) map[string]string {
	out := make(map[string]string, len(fields))
	for k, v := range fields {
		out["sys/class/dmi/id/"+k] = v + "\n"
	}
	return out
}

func TestDetectCloudHost(t *testing.T) {
	const ciPath = "run/cloud-init/instance-data.json"
	tests := []struct {
		name         string
		files        map[string]string
		goos         string
		wantDetected bool
		wantSource   string
		wantProvider string
	}{
		{name: "empty root", files: nil, wantDetected: false},
		{name: "gce dmi", files: dmi(map[string]string{"sys_vendor": "Google", "product_name": "Google Compute Engine"}),
			wantDetected: true, wantSource: "dmi", wantProvider: "gce"},
		{name: "google vendor without gce product is not a cloud", files: dmi(map[string]string{"sys_vendor": "Google", "product_name": "Pixelbook"}),
			wantDetected: false},
		{name: "aws sys_vendor", files: dmi(map[string]string{"sys_vendor": "Amazon EC2", "product_name": "m5.large"}),
			wantDetected: true, wantSource: "dmi", wantProvider: "aws"},
		{name: "aws bios_vendor", files: dmi(map[string]string{"bios_vendor": "Amazon EC2"}),
			wantDetected: true, wantSource: "dmi", wantProvider: "aws"},
		{name: "aws xen era", files: dmi(map[string]string{"sys_vendor": "Xen", "product_name": "HVM domU", "bios_version": "4.11.amazon"}),
			wantDetected: true, wantSource: "dmi", wantProvider: "aws"},
		{name: "plain xen guest is not a cloud", files: dmi(map[string]string{"sys_vendor": "Xen", "product_name": "HVM domU", "bios_version": "4.11"}),
			wantDetected: false},
		{name: "azure", files: dmi(map[string]string{"sys_vendor": "Microsoft Corporation", "product_name": "Virtual Machine", "chassis_asset_tag": azureChassisAssetTag}),
			wantDetected: true, wantSource: "dmi", wantProvider: "azure"},
		{name: "local hyper-v is not a cloud", files: dmi(map[string]string{"sys_vendor": "Microsoft Corporation", "product_name": "Virtual Machine", "chassis_asset_tag": "0000-0000"}),
			wantDetected: false},
		{name: "oracle", files: dmi(map[string]string{"sys_vendor": "QEMU", "chassis_asset_tag": "OracleCloud.com"}),
			wantDetected: true, wantSource: "dmi", wantProvider: "oracle"},
		{name: "alibaba", files: dmi(map[string]string{"sys_vendor": "Alibaba Cloud", "product_name": "Alibaba Cloud ECS"}),
			wantDetected: true, wantSource: "dmi", wantProvider: "alibaba"},
		{name: "openstack product", files: dmi(map[string]string{"sys_vendor": "RDO", "product_name": "OpenStack Compute"}),
			wantDetected: true, wantSource: "dmi", wantProvider: "openstack"},
		{name: "openstack nova", files: dmi(map[string]string{"product_name": "OpenStack Nova"}),
			wantDetected: true, wantSource: "dmi", wantProvider: "openstack"},
		{name: "digitalocean", files: dmi(map[string]string{"sys_vendor": "DigitalOcean", "product_name": "Droplet"}),
			wantDetected: true, wantSource: "dmi", wantProvider: "digitalocean"},
		{name: "hetzner", files: dmi(map[string]string{"sys_vendor": "Hetzner", "product_name": "vServer"}),
			wantDetected: true, wantSource: "dmi", wantProvider: "hetzner"},
		{name: "workstation dmi", files: dmi(map[string]string{"sys_vendor": "Dell Inc.", "product_name": "Precision 5570"}),
			wantDetected: false},
		{name: "local qemu", files: dmi(map[string]string{"sys_vendor": "QEMU", "product_name": "Standard PC (Q35 + ICH9, 2009)"}),
			wantDetected: false},
		{name: "generic kvm guest", files: dmi(map[string]string{"sys_vendor": "Red Hat", "product_name": "KVM", "bios_vendor": "SeaBIOS"}),
			wantDetected: false},
		{name: "generic qemu kvm guest", files: dmi(map[string]string{"sys_vendor": "QEMU", "product_name": "KVM Virtual Machine", "bios_vendor": "EFI Development Kit II / OVMF"}),
			wantDetected: false},
		{name: "cloud-init v1 cloud_name", files: map[string]string{ciPath: `{"v1":{"cloud_name":"aws"}}`},
			wantDetected: true, wantSource: "cloud-init", wantProvider: "aws"},
		{name: "cloud-init v1 cloud-name", files: map[string]string{ciPath: `{"v1":{"cloud-name":"gce"}}`},
			wantDetected: true, wantSource: "cloud-init", wantProvider: "gce"},
		{name: "cloud-init top-level cloud_name", files: map[string]string{ciPath: `{"cloud_name":"azure"}`},
			wantDetected: true, wantSource: "cloud-init", wantProvider: "azure"},
		{name: "cloud-init nocloud", files: map[string]string{ciPath: `{"v1":{"cloud_name":"nocloud"}}`}, wantDetected: false},
		{name: "cloud-init none", files: map[string]string{ciPath: `{"v1":{"cloud_name":"None"}}`}, wantDetected: false},
		{name: "cloud-init lxd", files: map[string]string{ciPath: `{"v1":{"cloud_name":"lxd"}}`}, wantDetected: false},
		{name: "cloud-init unknown", files: map[string]string{ciPath: `{"v1":{"cloud_name":"unknown"}}`}, wantDetected: false},
		{name: "cloud-init vmware", files: map[string]string{ciPath: `{"v1":{"cloud_name":"vmware"}}`}, wantDetected: false},
		{name: "cloud-init ovf", files: map[string]string{ciPath: `{"v1":{"cloud_name":"ovf"}}`}, wantDetected: false},
		{name: "cloud-init maas", files: map[string]string{ciPath: `{"v1":{"cloud_name":"maas"}}`}, wantDetected: false},
		{name: "cloud-init wsl", files: map[string]string{ciPath: `{"v1":{"cloud_name":"wsl"}}`}, wantDetected: false},
		{name: "cloud-init configdrive", files: map[string]string{ciPath: `{"v1":{"cloud_name":"configdrive"}}`}, wantDetected: false},
		{name: "cloud-init opennebula", files: map[string]string{ciPath: `{"v1":{"cloud_name":"opennebula"}}`}, wantDetected: false},
		{name: "cloud-init altcloud", files: map[string]string{ciPath: `{"v1":{"cloud_name":"altcloud"}}`}, wantDetected: false},
		{name: "cloud-init aliyun", files: map[string]string{ciPath: `{"v1":{"cloud_name":"aliyun"}}`},
			wantDetected: true, wantSource: "cloud-init", wantProvider: "aliyun"},
		{name: "cloud-init oracle", files: map[string]string{ciPath: `{"v1":{"cloud_name":"Oracle"}}`},
			wantDetected: true, wantSource: "cloud-init", wantProvider: "oracle"},
		{name: "cloud-init first name decides", files: map[string]string{ciPath: `{"v1":{"cloud_name":"vmware"},"cloud_name":"aws"}`}, wantDetected: false},
		{name: "cloud-init malformed", files: map[string]string{ciPath: `{not json`}, wantDetected: false},
		{name: "gce guest agent config", files: map[string]string{"etc/default/instance_configs.cfg": "[Accounts]\n"},
			wantDetected: true, wantSource: "guest-agent", wantProvider: "gce"},
		{name: "gce guest agent unit", files: map[string]string{"usr/lib/systemd/system/google-guest-agent.service": ""},
			wantDetected: true, wantSource: "guest-agent", wantProvider: "gce"},
		{name: "dmi wins over cloud-init", files: func() map[string]string {
			m := dmi(map[string]string{"sys_vendor": "Amazon EC2"})
			m[ciPath] = `{"v1":{"cloud_name":"gce"}}`
			return m
		}(), wantDetected: true, wantSource: "dmi", wantProvider: "aws"},
		{name: "non-linux is never detected", goos: "darwin",
			files:        dmi(map[string]string{"sys_vendor": "Google", "product_name": "Google Compute Engine"}),
			wantDetected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeFakeRoot(t, tt.files)
			goos := tt.goos
			if goos == "" {
				goos = "linux"
			}
			got := detectCloudHost(root, goos)
			if got.Detected != tt.wantDetected || got.Source != tt.wantSource || got.Provider != tt.wantProvider {
				t.Fatalf("detectCloudHost = %+v, want detected=%v source=%q provider=%q",
					got, tt.wantDetected, tt.wantSource, tt.wantProvider)
			}
		})
	}
}

func TestParseHostNetworkBlockPolicy(t *testing.T) {
	tests := []struct {
		in        string
		want      string
		wantKnown bool
	}{
		{"", HostNetworkBlockPolicyAuto, true},
		{"auto", HostNetworkBlockPolicyAuto, true},
		{" Strict ", HostNetworkBlockPolicyStrict, true},
		{"ALLOW", HostNetworkBlockPolicyAllow, true},
		{"off", HostNetworkBlockPolicyStrict, false},
		{"true", HostNetworkBlockPolicyStrict, false},
	}
	for _, tt := range tests {
		got, known := ParseHostNetworkBlockPolicy(tt.in)
		if got != tt.want || known != tt.wantKnown {
			t.Errorf("ParseHostNetworkBlockPolicy(%q) = %q, %v; want %q, %v", tt.in, got, known, tt.want, tt.wantKnown)
		}
	}
}

func TestDecideHostNetworkBlock(t *testing.T) {
	cloud := CloudHost{Detected: true, Source: "dmi", Provider: "gce"}
	none := CloudHost{}
	tests := []struct {
		name   string
		policy string
		host   CloudHost
		want   HostNetworkBlockAction
	}{
		{"auto on cloud refuses", HostNetworkBlockPolicyAuto, cloud, HostNetworkBlockRefuse},
		{"auto off cloud warns", HostNetworkBlockPolicyAuto, none, HostNetworkBlockWarn},
		{"strict on cloud refuses", HostNetworkBlockPolicyStrict, cloud, HostNetworkBlockRefuse},
		{"strict off cloud refuses", HostNetworkBlockPolicyStrict, none, HostNetworkBlockRefuse},
		{"allow on cloud acknowledges", HostNetworkBlockPolicyAllow, cloud, HostNetworkBlockAllowAcknowledged},
		{"allow off cloud warns", HostNetworkBlockPolicyAllow, none, HostNetworkBlockWarn},
		{"unknown policy refuses", "bogus", none, HostNetworkBlockRefuse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DecideHostNetworkBlock(tt.policy, tt.host); got != tt.want {
				t.Fatalf("DecideHostNetworkBlock(%q, %+v) = %v, want %v", tt.policy, tt.host, got, tt.want)
			}
		})
	}
}
