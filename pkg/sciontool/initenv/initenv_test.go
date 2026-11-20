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

package initenv

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
)

func TestLoad_UnsetsMarkerAndCombinesWithStaticDenylist(t *testing.T) {
	t.Setenv(envpolicy.HookWithheldEnvVar, "BUNDLE_KEY,PATH")
	t.Setenv("SCION_HOOK_UNKNOWN_FUTURE_VAR", "ignored")

	pol := Load()

	if _, present := os.LookupEnv(envpolicy.HookWithheldEnvVar); present {
		t.Fatalf("%s was not unset by Load()", envpolicy.HookWithheldEnvVar)
	}
	if pol.Mode != envpolicy.HookEnvModeDenylist {
		t.Errorf("Mode = %q, want %q", pol.Mode, envpolicy.HookEnvModeDenylist)
	}
	for _, want := range []string{"BUNDLE_KEY", "PATH", "LD_PRELOAD", "GIT_CONFIG_COUNT"} {
		if !pol.Withheld.Withholds(want) {
			t.Errorf("Withheld.Withholds(%q) = false, want true", want)
		}
	}
	if pol.Withheld.Withholds("OPERATOR_KEY") {
		t.Error("Withheld.Withholds(OPERATOR_KEY) = true, want false")
	}
}

func TestLoad_WithoutMarkerAppliesStaticDenylistOnly(t *testing.T) {
	t.Setenv(envpolicy.HookWithheldEnvVar, "")
	_ = os.Unsetenv(envpolicy.HookWithheldEnvVar)

	pol := Load()

	if pol.Mode != envpolicy.HookEnvModeDenylist {
		t.Errorf("Mode = %q, want %q", pol.Mode, envpolicy.HookEnvModeDenylist)
	}
	if !pol.Withheld.Withholds("LD_PRELOAD") {
		t.Error("expected static denylist entry LD_PRELOAD to be withheld")
	}
	if pol.Withheld.Withholds("BUNDLE_KEY") {
		t.Error("expected BUNDLE_KEY not to be withheld when marker is absent")
	}
	if got := pol.Withheld.Extra(); len(got) != 0 {
		t.Errorf("Extra() = %v, want empty", got)
	}
}

func TestStartupChildEnv_ExactFromScratchSetAndWithheldFiltering(t *testing.T) {
	ambient := map[string]string{
		"PATH":                "/evil/bin",
		"HOME":                "/evil/home",
		"LD_PRELOAD":          "/tmp/evil.so",
		"GIT_CONFIG_COUNT":    "1",
		"GIT_SSH_COMMAND":     "/tmp/evil-ssh",
		"GITHUB_TOKEN":        "ambient-token-must-not-leak",
		"CUSTOM_AMBIENT":      "must-not-leak",
		"GIT_SSL_CAINFO":      "/etc/ssl/bundle-ca.pem",
		"SSL_CERT_FILE":       "/etc/ssl/operator-ca.pem",
		"CURL_CA_BUNDLE":      "",
		"HTTPS_PROXY":         "http://bundle-proxy:8080",
		"https_proxy":         "http://operator-proxy:8080",
		"NO_PROXY":            "localhost,127.0.0.1",
		"no_proxy":            "internal.local",
		"ALL_PROXY":           "socks5://proxy:1080",
		"all_proxy":           "",
		"HTTP_PROXY":          "http://http-proxy:8080",
		"http_proxy":          "http://http-proxy-lower:8080",
		"NODE_EXTRA_CA_CERTS": "/etc/ssl/node-ca.pem",
	}
	getenv := func(name string) string { return ambient[name] }

	pol := Policy{
		Mode:     envpolicy.HookEnvModeDenylist,
		Withheld: envpolicy.NewSet("GIT_SSL_CAINFO", "HTTPS_PROXY"),
	}

	got := pol.startupChildEnvFrom(getenv, "/home/scion", "GITHUB_TOKEN=explicit-token")

	want := []string{
		"PATH=" + strings.Join(rootexec.SearchPath, ":"),
		"HOME=/home/scion",
		"LANG=C.UTF-8",
		"GIT_TERMINAL_PROMPT=0",
		"NODE_EXTRA_CA_CERTS=/etc/ssl/node-ca.pem",
		"SSL_CERT_FILE=/etc/ssl/operator-ca.pem",
		"HTTP_PROXY=http://http-proxy:8080",
		"http_proxy=http://http-proxy-lower:8080",
		"https_proxy=http://operator-proxy:8080",
		"NO_PROXY=localhost,127.0.0.1",
		"no_proxy=internal.local",
		"ALL_PROXY=socks5://proxy:1080",
		"GITHUB_TOKEN=explicit-token",
	}

	if !slices.Equal(got, want) {
		t.Errorf("startupChildEnvFrom() =\n%v\nwant:\n%v", got, want)
	}
}

func TestStartupChildEnv_EmptyHomeDefaultsToRoot(t *testing.T) {
	pol := Policy{}
	got := pol.startupChildEnvFrom(func(string) string { return "" }, "")
	want := []string{
		"PATH=" + strings.Join(rootexec.SearchPath, ":"),
		"HOME=/root",
		"LANG=C.UTF-8",
		"GIT_TERMINAL_PROMPT=0",
	}
	if !slices.Equal(got, want) {
		t.Errorf("startupChildEnvFrom(empty home) = %v, want %v", got, want)
	}
}
