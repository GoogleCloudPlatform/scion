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
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hostlimits"
)

// inotifyReader is a Linux hostlimits.Reader whose /proc serves the two
// inotify limits (an empty value is a missing file) plus the files of
// extra (a fake process table for the usage scan).
func inotifyReader(instances, watches string, extra fstest.MapFS) hostlimits.Reader {
	proc := fstest.MapFS{}
	for name, f := range extra {
		proc[name] = f
	}
	if instances != "" {
		proc["sys/fs/inotify/max_user_instances"] = &fstest.MapFile{Data: []byte(instances + "\n")}
	}
	if watches != "" {
		proc["sys/fs/inotify/max_user_watches"] = &fstest.MapFile{Data: []byte(watches + "\n")}
	}
	return hostlimits.Reader{GOOS: "linux", Proc: proc}
}

func TestBrokerInotifyWarning(t *testing.T) {
	low := inotifyReader("128", "524288", nil)
	cases := []struct {
		name           string
		hosted         bool
		hostUserAgents bool
		r              hostlimits.Reader
		wantWarn       bool
	}{
		{name: "hosted, host-user agents, low", hosted: true, hostUserAgents: true, r: low, wantWarn: true},
		{name: "workstation, host-user agents, low", hosted: false, hostUserAgents: true, r: low},
		{name: "hosted, agents elsewhere, low", hosted: true, hostUserAgents: false, r: low},
		{name: "hosted, host-user agents, fine", hosted: true, hostUserAgents: true, r: inotifyReader("8192", "524288", nil)},
		{name: "hosted, host-user agents, files missing", hosted: true, hostUserAgents: true, r: inotifyReader("", "", nil)},
		{name: "hosted, host-user agents, not linux", hosted: true, hostUserAgents: true, r: hostlimits.Reader{GOOS: "darwin", Proc: low.Proc}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := brokerInotifyWarning(c.hosted, c.hostUserAgents, c.r)
			if !c.wantWarn {
				if got != "" {
					t.Fatalf("unexpected warning: %q", got)
				}
				return
			}
			if strings.Contains(got, "\n") {
				t.Fatalf("warning is not one line: %q", got)
			}
			for _, want := range []string{"fs.inotify.max_user_instances is 128 (want at least 228)", "assuming the default capacity", "sudo sysctl -w fs.inotify.max_user_instances=8192"} {
				if !strings.Contains(got, want) {
					t.Fatalf("warning %q lacks %q", got, want)
				}
			}
		})
	}
}

func TestBrokerAgentsShareHostUser(t *testing.T) {
	docker := config.V1RuntimeBrokerInstanceConfig{Key: "d", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeDocker}}
	k8s := config.V1RuntimeBrokerInstanceConfig{Key: "k", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeKubernetes}}
	none := config.V1RuntimeBrokerInstanceConfig{Key: "n"}
	cases := []struct {
		name           string
		instances      []config.V1RuntimeBrokerInstanceConfig
		defaultRuntime string
		want           bool
	}{
		{name: "flat: kubernetes and unset target", instances: []config.V1RuntimeBrokerInstanceConfig{k8s, none}, defaultRuntime: "docker", want: false},
		{name: "flat: with a docker instance", instances: []config.V1RuntimeBrokerInstanceConfig{k8s, none, docker}, defaultRuntime: "kubernetes", want: true},
		{name: "legacy docker", defaultRuntime: "docker", want: true},
		{name: "legacy podman", defaultRuntime: "podman", want: true},
		{name: "legacy unknown runtime", defaultRuntime: "", want: true},
		{name: "legacy kubernetes", defaultRuntime: "kubernetes", want: false},
		{name: "legacy cloudrun", defaultRuntime: "cloudrun", want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := brokerAgentsShareHostUser(c.instances, c.defaultRuntime); got != c.want {
				t.Fatalf("brokerAgentsShareHostUser = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCheckDoctorInotifyLimits(t *testing.T) {
	proc := fstest.MapFS{
		"10/status":   {Data: []byte("Name:\tnode\nUid:\t1000\t1000\t1000\t1000\n")},
		"10/fd/3":     {Data: []byte("anon_inode:inotify"), Mode: fs.ModeSymlink},
		"10/fdinfo/3": {Data: []byte("inotify wd:1 ino:2\ninotify wd:2 ino:3\n")},
	}
	const usage = "; in use by processes of uid 1000: approximately 1 instances, 2 watches (a descriptor shared by several processes is counted once per process)"
	t.Run("broker host, low", func(t *testing.T) {
		c := checkDoctorInotifyLimits("", inotifyReader("128", "8192", proc), 1000)
		if c.Name != "inotify-limits" || c.Status != "warn" {
			t.Fatalf("got %+v, want inotify-limits warn", c)
		}
		if !strings.HasPrefix(c.Message, "Low for about 100 agents") || !strings.Contains(c.Message, "fs.inotify.max_user_instances is 128") || !strings.HasSuffix(c.Message, usage) {
			t.Fatalf("message %q", c.Message)
		}
		if !strings.Contains(c.Remediation, "sudo sysctl -w fs.inotify.max_user_instances=8192 fs.inotify.max_user_watches=524288") {
			t.Fatalf("remediation %q", c.Remediation)
		}
	})
	t.Run("broker host, fine", func(t *testing.T) {
		c := checkDoctorInotifyLimits("", inotifyReader("8192", "524288", proc), 1000)
		if c.Status != "pass" || c.Remediation != "" || !strings.HasSuffix(c.Message, usage) {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("not a broker host, low", func(t *testing.T) {
		c := checkDoctorInotifyLimits("not a broker host", inotifyReader("128", "8192", proc), 1000)
		if c.Status != "skip" || c.Remediation != "" {
			t.Fatalf("got %+v, want a skip without remediation", c)
		}
		if !strings.HasPrefix(c.Message, "Not judged (not a broker host): ") || !strings.Contains(c.Message, "fs.inotify.max_user_instances is 128") || !strings.HasSuffix(c.Message, usage) {
			t.Fatalf("message %q", c.Message)
		}
	})
	t.Run("no processes of the uid", func(t *testing.T) {
		c := checkDoctorInotifyLimits("", inotifyReader("8192", "524288", proc), 4242)
		if c.Status != "pass" || !strings.HasSuffix(c.Message, "; in use by processes of uid 4242: approximately 0 instances, 0 watches (a descriptor shared by several processes is counted once per process)") {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("not applicable", func(t *testing.T) {
		for _, notJudged := range []string{"", "not a broker host"} {
			c := checkDoctorInotifyLimits(notJudged, inotifyReader("", "", proc), 1000)
			if c.Status != "skip" || c.Message != "Not applicable (/proc/sys/fs/inotify is not readable)" {
				t.Fatalf("notJudged %q: got %+v", notJudged, c)
			}
		}
	})
}
