//go:build unix

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

package commands

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/initenv"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
)

func TestConfigureGitCommandWithPolicy_FromScratchEnvAndWithheldFiltering(t *testing.T) {
	// Clear trust-bundle and proxy vars first so container environment does not
	// leak extra entries into the exact comparison.
	for _, k := range []string{
		"NODE_EXTRA_CA_CERTS", "GIT_SSL_CAINFO", "SSL_CERT_FILE", "CURL_CA_BUNDLE", "SSL_CERT_DIR",
		"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy", "ALL_PROXY", "all_proxy",
	} {
		t.Setenv(k, "")
	}

	t.Setenv("LD_PRELOAD", "/tmp/evil.so")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_SSH_COMMAND", "/tmp/evil-ssh")
	t.Setenv("GITHUB_TOKEN", "ambient-token")
	t.Setenv("CUSTOM_AMBIENT", "must-not-inherit")
	t.Setenv("GIT_SSL_CAINFO", "/etc/ssl/withheld-ca.pem")
	t.Setenv("SSL_CERT_FILE", "/etc/ssl/operator-ca.pem")
	t.Setenv("HTTPS_PROXY", "http://withheld-proxy:8080")
	t.Setenv("https_proxy", "http://operator-proxy:8080")
	t.Setenv("NO_PROXY", "localhost")

	pol := initenv.Policy{
		Mode:     envpolicy.HookEnvModeDenylist,
		Withheld: envpolicy.NewSet("GIT_SSL_CAINFO", "HTTPS_PROXY"),
	}

	t.Run("uid>0 uses agentHome and excludes ambient/withheld vars", func(t *testing.T) {
		cmd := exec.CommandContext(context.Background(), "git", "status")
		configureGitCommandWithPolicy(cmd, os.Getuid(), os.Getgid(), "/home/scion", pol)
		want := []string{
			"PATH=" + strings.Join(rootexec.SearchPath, ":"),
			"HOME=/home/scion",
			"LANG=C.UTF-8",
			"GIT_TERMINAL_PROMPT=0",
			"SSL_CERT_FILE=/etc/ssl/operator-ca.pem",
			"https_proxy=http://operator-proxy:8080",
			"NO_PROXY=localhost",
		}
		if !slices.Equal(cmd.Env, want) {
			t.Errorf("cmd.Env =\n%v\nwant:\n%v", cmd.Env, want)
		}
	})

	t.Run("uid<=0 uses /root and extra appends GITHUB_TOKEN", func(t *testing.T) {
		cmd := exec.CommandContext(context.Background(), "git", "fetch")
		configureGitCommandWithPolicy(cmd, 0, 0, "/home/scion", pol, "GITHUB_TOKEN=explicit-token")
		want := []string{
			"PATH=" + strings.Join(rootexec.SearchPath, ":"),
			"HOME=/root",
			"LANG=C.UTF-8",
			"GIT_TERMINAL_PROMPT=0",
			"SSL_CERT_FILE=/etc/ssl/operator-ca.pem",
			"https_proxy=http://operator-proxy:8080",
			"NO_PROXY=localhost",
			"GITHUB_TOKEN=explicit-token",
		}
		if !slices.Equal(cmd.Env, want) {
			t.Errorf("cmd.Env =\n%v\nwant:\n%v", cmd.Env, want)
		}
	})
}

func TestGitCloneWorkspaceWithPolicy_TokenOnlyOnAuthenticatingCommandsAndCleanEnv(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if os.Geteuid() == 0 {
		t.Skip("runs as root; privilege-drop path differs")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}

	for _, enforced := range []bool{false, true} {
		name := "enforced=false"
		if enforced {
			name = "enforced=true"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			binDir := filepath.Join(root, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			envLog := filepath.Join(root, "git-env.log")
			// Fake git records subcommand + env for each invocation.
			// For fetch and ls-remote, fail immediately without hitting the
			// network (failing fetch causes gitCloneWorkspace to invoke
			// ls-remote via detectDefaultBranch, exercising both auth paths).
			script := "#!/bin/sh\n" +
				"{\n" +
				"  printf '===ARGS:%s\\n' \"$*\"\n" +
				"  env\n" +
				"  printf '===END\\n'\n" +
				"} >> '" + envLog + "'\n" +
				"for a in \"$@\"; do if [ \"$a\" = fetch ] || [ \"$a\" = ls-remote ]; then exit 1; fi; done\n" +
				"exec '" + realGit + "' \"$@\"\n"
			if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			origResolve := resolveGitForInit
			resolveGitForInit = func(string) (string, error) { return filepath.Join(binDir, "git"), nil }
			t.Cleanup(func() { resolveGitForInit = origResolve })

			workspace := filepath.Join(root, "workspace")
			if err := os.MkdirAll(workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			agentHome := filepath.Join(root, "home")
			if err := os.MkdirAll(agentHome, 0o755); err != nil {
				t.Fatal(err)
			}

			t.Setenv("SCION_GIT_CLONE_URL", "https://github.com/org/repo.git")
			t.Setenv(api.EnvGitTokenRemote, "github.com/org/repo")
			t.Setenv("SCION_WORKSPACE_PATH", workspace)
			t.Setenv("SCION_GIT_BRANCH", "nonexistent-branch")
			t.Setenv("SCION_GIT_DEPTH", "1")
			t.Setenv("GITHUB_TOKEN", "secret-auth-token")
			t.Setenv("SCION_AGENT_BRANCH", "")
			t.Setenv("SCION_AGENT_NAME", "test-agent")
			t.Setenv("LD_PRELOAD", "/tmp/evil.so")
			t.Setenv("GIT_CONFIG_COUNT", "1")
			t.Setenv("CUSTOM_AMBIENT", "must-not-appear")
			t.Setenv("GIT_SSL_CAINFO", "/etc/ssl/withheld.pem")
			t.Setenv("SSL_CERT_FILE", "/etc/ssl/operator.pem")
			t.Setenv("HTTPS_PROXY", "http://withheld-proxy:8080")
			t.Setenv("https_proxy", "http://operator-proxy:8080")

			pol := initenv.Policy{
				Mode:     envpolicy.HookEnvModeDenylist,
				Withheld: envpolicy.NewSet("GIT_SSL_CAINFO", "HTTPS_PROXY"),
			}

			// Expect error because fake git fails fetch/ls-remote.
			_ = gitCloneWorkspaceWithPolicy(os.Getuid(), os.Getgid(), agentHome, enforced, pol)

			raw, err := os.ReadFile(envLog)
			if err != nil {
				t.Fatalf("read git-env.log: %v", err)
			}
			blocks := strings.Split(string(raw), "===END\n")
			var sawInit, sawRemote, sawFetch, sawLsRemote bool
			for _, block := range blocks {
				block = strings.TrimSpace(block)
				if block == "" {
					continue
				}
				lines := strings.Split(block, "\n")
				argsLine := strings.TrimPrefix(lines[0], "===ARGS:")
				envMap := map[string]string{}
				for _, l := range lines[1:] {
					if k, v, ok := strings.Cut(l, "="); ok {
						envMap[k] = v
					}
				}

				isAuth := strings.Contains(argsLine, " fetch") || strings.HasPrefix(argsLine, "fetch") ||
					strings.Contains(argsLine, " ls-remote") || strings.HasPrefix(argsLine, "ls-remote")
				if strings.HasPrefix(argsLine, "init ") {
					sawInit = true
				}
				if strings.Contains(argsLine, "remote add") {
					sawRemote = true
				}
				if strings.Contains(argsLine, " fetch") {
					sawFetch = true
				}
				if strings.Contains(argsLine, " ls-remote") {
					sawLsRemote = true
				}

				if envMap["GIT_TERMINAL_PROMPT"] != "0" {
					t.Errorf("args %q: GIT_TERMINAL_PROMPT = %q, want 0", argsLine, envMap["GIT_TERMINAL_PROMPT"])
				}
				if envMap["LANG"] != "C.UTF-8" {
					t.Errorf("args %q: LANG = %q, want C.UTF-8", argsLine, envMap["LANG"])
				}
				if envMap["HOME"] != agentHome {
					t.Errorf("args %q: HOME = %q, want %q", argsLine, envMap["HOME"], agentHome)
				}
				if envMap["SSL_CERT_FILE"] != "/etc/ssl/operator.pem" {
					t.Errorf("args %q: SSL_CERT_FILE = %q, want /etc/ssl/operator.pem", argsLine, envMap["SSL_CERT_FILE"])
				}
				if envMap["https_proxy"] != "http://operator-proxy:8080" {
					t.Errorf("args %q: https_proxy = %q, want http://operator-proxy:8080", argsLine, envMap["https_proxy"])
				}
				for _, forbidden := range []string{"LD_PRELOAD", "GIT_CONFIG_COUNT", "CUSTOM_AMBIENT", "GIT_SSL_CAINFO", "HTTPS_PROXY"} {
					if v, ok := envMap[forbidden]; ok {
						t.Errorf("args %q: forbidden/withheld var %s=%q leaked into git env", argsLine, forbidden, v)
					}
				}
				if isAuth {
					if envMap["GITHUB_TOKEN"] != "secret-auth-token" {
						t.Errorf("auth args %q: GITHUB_TOKEN = %q, want secret-auth-token", argsLine, envMap["GITHUB_TOKEN"])
					}
				} else {
					if v, ok := envMap["GITHUB_TOKEN"]; ok {
						t.Errorf("non-auth args %q: GITHUB_TOKEN=%q must not be passed to non-authenticating git command", argsLine, v)
					}
				}
			}
			if !sawInit || !sawRemote || !sawFetch || !sawLsRemote {
				t.Fatalf("missing expected git phases: init=%v remote=%v fetch=%v ls-remote=%v\nlog:\n%s",
					sawInit, sawRemote, sawFetch, sawLsRemote, raw)
			}
		})
	}
}

func TestGitCloneWorkspace_NeverConsultsPATHForGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if os.Geteuid() == 0 {
		t.Skip("runs as root; privilege-drop path differs")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "planted-git-ran")
	fake := filepath.Join(dir, "git")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCION_GIT_CLONE_URL", "file:///nonexistent-repo-for-path-test")
	t.Setenv("SCION_WORKSPACE_PATH", workspace)
	t.Setenv("SCION_GIT_BRANCH", "main")
	t.Setenv("SCION_GIT_DEPTH", "1")
	t.Setenv("SCION_AGENT_NAME", "test-agent")

	_ = gitCloneWorkspace(os.Getuid(), os.Getgid(), dir, false)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("gitCloneWorkspace executed a planted git from $PATH")
	}
}

func TestConfigureSharedWorkspaceGitWithPolicy_FromScratchEnvAcrossDropAndNonEnforced(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		uid, gid int
		enforced bool
		wantHome string
	}{
		{
			name:     "uid<=0 non-enforced sets explicit non-nil from-scratch env",
			uid:      0,
			gid:      0,
			enforced: false,
			wantHome: "/root",
		},
		{
			name:     "usable uid/gid non-enforced",
			uid:      os.Getuid(),
			gid:      os.Getgid(),
			enforced: false,
			wantHome: "AGENT_HOME",
		},
		{
			name:     "usable uid/gid enforced",
			uid:      os.Getuid(),
			gid:      os.Getgid(),
			enforced: true,
			wantHome: "AGENT_HOME",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantHome == "AGENT_HOME" && (tc.uid <= 0 || tc.gid <= 0) {
				t.Skip("test process uid/gid is not a usable non-root pair")
			}
			root := t.TempDir()
			binDir := filepath.Join(root, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			envLog := filepath.Join(root, "git-env.log")
			script := "#!/bin/sh\n" +
				"{\n" +
				"  printf '===CALL\\n'\n" +
				"  env\n" +
				"  printf '===END\\n'\n" +
				"} >> '" + envLog + "'\n" +
				"exec '" + realGit + "' \"$@\"\n"
			if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			origResolve := resolveGitForInit
			resolveGitForInit = func(string) (string, error) { return filepath.Join(binDir, "git"), nil }
			t.Cleanup(func() { resolveGitForInit = origResolve })

			agentHome := filepath.Join(root, "home")
			if err := os.MkdirAll(agentHome, 0o755); err != nil {
				t.Fatal(err)
			}
			wantHome := tc.wantHome
			if wantHome == "AGENT_HOME" {
				wantHome = agentHome
			}

			t.Setenv(api.EnvGitTokenRemote, "github.com/org/shared")
			t.Setenv("SCION_AGENT_NAME", "test-agent")
			t.Setenv("GITHUB_TOKEN", "must-not-reach-git-config")
			t.Setenv("LD_PRELOAD", "/tmp/evil.so")
			t.Setenv("GIT_CONFIG_COUNT", "1")
			t.Setenv("CUSTOM_AMBIENT", "must-not-appear")
			t.Setenv("GIT_SSL_CAINFO", "/etc/ssl/withheld.pem")
			t.Setenv("SSL_CERT_FILE", "/etc/ssl/operator.pem")
			t.Setenv("HTTPS_PROXY", "http://withheld-proxy:8080")
			t.Setenv("https_proxy", "http://operator-proxy:8080")

			pol := initenv.Policy{
				Mode:     envpolicy.HookEnvModeDenylist,
				Withheld: envpolicy.NewSet("GIT_SSL_CAINFO", "HTTPS_PROXY"),
			}

			if err := configureSharedWorkspaceGitWithPolicy(agentHome, tc.uid, tc.gid, tc.enforced, pol); err != nil {
				t.Fatalf("configureSharedWorkspaceGitWithPolicy: %v", err)
			}

			raw, err := os.ReadFile(envLog)
			if err != nil {
				t.Fatalf("read git-env.log: %v", err)
			}
			blocks := strings.Split(string(raw), "===END\n")
			calls := 0
			for _, block := range blocks {
				block = strings.TrimSpace(block)
				if block == "" {
					continue
				}
				calls++
				lines := strings.Split(block, "\n")
				envMap := map[string]string{}
				for _, l := range lines[1:] {
					if k, v, ok := strings.Cut(l, "="); ok {
						envMap[k] = v
					}
				}
				if envMap["HOME"] != wantHome {
					t.Errorf("call %d: HOME = %q, want %q", calls, envMap["HOME"], wantHome)
				}
				if envMap["GIT_TERMINAL_PROMPT"] != "0" {
					t.Errorf("call %d: GIT_TERMINAL_PROMPT = %q, want 0", calls, envMap["GIT_TERMINAL_PROMPT"])
				}
				if envMap["LANG"] != "C.UTF-8" {
					t.Errorf("call %d: LANG = %q, want C.UTF-8", calls, envMap["LANG"])
				}
				if envMap["SSL_CERT_FILE"] != "/etc/ssl/operator.pem" {
					t.Errorf("call %d: SSL_CERT_FILE = %q, want /etc/ssl/operator.pem", calls, envMap["SSL_CERT_FILE"])
				}
				if envMap["https_proxy"] != "http://operator-proxy:8080" {
					t.Errorf("call %d: https_proxy = %q, want http://operator-proxy:8080", calls, envMap["https_proxy"])
				}
				for _, forbidden := range []string{"GITHUB_TOKEN", "LD_PRELOAD", "GIT_CONFIG_COUNT", "CUSTOM_AMBIENT", "GIT_SSL_CAINFO", "HTTPS_PROXY"} {
					if v, ok := envMap[forbidden]; ok {
						t.Errorf("call %d: forbidden/withheld var %s=%q leaked into git env", calls, forbidden, v)
					}
				}
			}
			if calls == 0 {
				t.Fatal("expected at least one git config invocation")
			}
		})
	}
}
