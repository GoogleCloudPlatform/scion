package main

import (
	"bufio"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// validEnv returns the required variables with valid values; everything else
// falls back to its default.
func validEnv() map[string]string {
	return map[string]string{
		EnvTargetRepos:        "example-owner/example-repo",
		EnvGitHubAppID:        "123456",
		EnvGitHubInstallScope: "repo",
		EnvGCPProject:         "example-project",
		EnvRegion:             "us-central1",
	}
}

func getenvFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(getenvFrom(validEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := &Config{
		TargetRepos:             []Repo{{Owner: "example-owner", Name: "example-repo"}},
		GitHubAppID:             123456,
		InstallScope:            ScopeRepo,
		GCPProject:              "example-project",
		Region:                  "us-central1",
		RunnerLabel:             "scion-cr-8c32g",
		RunnerJobName:           "cr-gh-runner-8c32g",
		RunnerImageName:         "gh-runner",
		MediatorImageName:       "cr-runner-mediator",
		RunnerCPU:               8,
		RunnerMemory:            "32Gi",
		RunnerMemoryMi:          32 * 1024,
		RunnerDiskGi:            20,
		MaxConcurrentRunners:    5,
		TaskTimeout:             45 * time.Minute,
		ArtifactRepo:            "cr-runners",
		MediatorServiceName:     "cr-runner-mediator",
		ReconcilerJobName:       "cr-runner-reconciler",
		ReconcileSchedule:       "*/5 * * * *",
		RunnerGroupID:           1,
		RoutingVariableName:     "SCION_LARGE_RUNNER",
		BuildSAName:             "cr-runner-build-sa",
		MediatorSAName:          "cr-runner-mediator-sa",
		SchedulerSAName:         "cr-runner-scheduler-sa",
		ExecSAName:              "cr-runner-exec-sa",
		AppPrivateKeySecretName: "cr-runner-gh-app-private-key",
		WebhookSecretName:       "cr-runner-gh-webhook-secret",
		ClaimTTL:                300 * time.Second,
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("Load defaults:\n got %+v\nwant %+v", c, want)
	}
}

func TestLoadOverridesAndMultipleRepos(t *testing.T) {
	env := validEnv()
	env[EnvTargetRepos] = " owner-a/repo.one , Owner-B/repo_two "
	env[EnvRunnerCPU] = "4"
	env[EnvRunnerMemory] = "16384Mi"
	env[EnvRunnerDiskGi] = "100"
	env[EnvTaskTimeout] = "2h"
	env[EnvMaxConcurrentRunners] = "20"
	c, err := Load(getenvFrom(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantRepos := []Repo{{"owner-a", "repo.one"}, {"Owner-B", "repo_two"}}
	if !reflect.DeepEqual(c.TargetRepos, wantRepos) {
		t.Errorf("TargetRepos = %v, want %v", c.TargetRepos, wantRepos)
	}
	if c.RunnerCPU != 4 || c.RunnerMemoryMi != 16384 || c.RunnerDiskGi != 100 || c.TaskTimeout != 2*time.Hour || c.MaxConcurrentRunners != 20 {
		t.Errorf("overrides not applied: %+v", c)
	}
}

func TestLoadOwnerLength(t *testing.T) {
	env := validEnv()
	env[EnvTargetRepos] = strings.Repeat("a", 39) + "/repo," + strings.Repeat("a-", 19) + "a/repo"
	if _, err := Load(getenvFrom(env)); err != nil {
		t.Fatalf("39-char owners should load: %v", err)
	}
}

func TestLoadMediatorURL(t *testing.T) {
	env := validEnv()
	env[EnvMediatorURL] = "https://cr-runner-mediator-abc123-uc.a.run.app/"
	c, err := Load(getenvFrom(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := "https://cr-runner-mediator-abc123-uc.a.run.app"; c.MediatorURL != want {
		t.Errorf("MediatorURL = %q, want %q (trailing slash trimmed)", c.MediatorURL, want)
	}
}

func TestLoadOrgScope(t *testing.T) {
	env := validEnv()
	env[EnvGitHubInstallScope] = "org"
	env[EnvTargetRepos] = "some-org/a,SOME-ORG/b"
	if _, err := Load(getenvFrom(env)); err != nil {
		t.Fatalf("same-owner org scope should load: %v", err)
	}
	env[EnvTargetRepos] = "some-org/a,other-user/a"
	_, err := Load(getenvFrom(env))
	if err == nil || !strings.Contains(err.Error(), EnvGitHubInstallScope) {
		t.Fatalf("mixed owners with org scope: err = %v, want %s error", err, EnvGitHubInstallScope)
	}
}

func TestLoadRequired(t *testing.T) {
	_, err := Load(getenvFrom(nil))
	if err == nil {
		t.Fatal("Load with empty env succeeded")
	}
	for _, k := range []string{EnvTargetRepos, EnvGitHubAppID, EnvGitHubInstallScope, EnvGCPProject, EnvRegion} {
		if !strings.Contains(err.Error(), k+": required") {
			t.Errorf("error does not report %s as required: %v", k, err)
		}
	}
}

func TestLoadInvalid(t *testing.T) {
	tests := []struct {
		key, value string
	}{
		{EnvTargetRepos, "no-slash"},
		{EnvTargetRepos, "a/b,,c/d"},
		{EnvTargetRepos, "a/b,A/B"},
		{EnvTargetRepos, "-bad/repo"},
		{EnvTargetRepos, "owner/.."},
		{EnvTargetRepos, "owner/re po"},
		{EnvTargetRepos, "a--b/repo"},
		{EnvTargetRepos, "owner-/repo"},
		{EnvTargetRepos, strings.Repeat("a", 40) + "/repo"},
		{EnvTargetRepos, strings.Repeat("a-", 20) + "a/repo"}, // 41 chars, alternating hyphens
		{EnvGitHubAppID, "abc"},
		{EnvGitHubAppID, "0"},
		{EnvGitHubInstallScope, "enterprise"},
		{EnvGCPProject, "Bad_Project"},
		{EnvGCPProject, "short"},
		{EnvRegion, "uscentral"},
		{EnvRunnerLabel, "has space"},
		{EnvRunnerLabel, "a,b"},
		{EnvRunnerLabel, "self-hosted"},
		{EnvRunnerLabel, "Linux"},
		{EnvRunnerJobName, "Bad-Name"},
		{EnvRunnerJobName, "ends-with-"},
		{EnvRunnerImageName, "Image"},
		{EnvMediatorImageName, "Mediator"},
		{EnvMediatorImageName, "gh-runner"}, // duplicates the runner image
		{EnvRunnerCPU, "3"},
		{EnvRunnerCPU, "0"},
		{EnvRunnerCPU, "x"},
		{EnvRunnerMemory, "32G"},
		{EnvRunnerMemory, "33Gi"},
		{EnvRunnerMemory, "256Mi"},
		{EnvRunnerMemory, "18014398509481985Gi"}, // overflows to 1024Mi if multiplied first
		{EnvRunnerMemory, "99999999999999999999Mi"},
		{EnvRunnerMemory, "2Gi"}, // below the 8-CPU floor of 4Gi
		{EnvRunnerDiskGi, "0"},
		{EnvRunnerDiskGi, "101"},
		{EnvMaxConcurrentRunners, "-1"},
		{EnvTaskTimeout, "45"},
		{EnvTaskTimeout, "30s"},
		{EnvTaskTimeout, "169h"},
		{EnvClaimTTL, "300"},
		{EnvClaimTTL, "10s"},
		{EnvClaimTTL, "2h"},
		{EnvArtifactRepo, "Repo"},
		{EnvMediatorServiceName, "1abc"},
		{EnvReconcilerJobName, "has_underscore"},
		{EnvReconcileSchedule, "@every 5m"},
		{EnvReconcileSchedule, "*/5 * * *"},
		{EnvReconcileSchedule, "*/5 * * * * *"},
		{EnvMediatorURL, "http://mediator.example.com"},
		{EnvMediatorURL, "https://"},
		{EnvMediatorURL, "https://mediator.example.com/webhook"},
		{EnvMediatorURL, "https://user@mediator.example.com"},
		{EnvMediatorURL, "https://mediator.example.com?x=1"},
		{EnvMediatorURL, "https://mediator.example.com?"},
		{EnvMediatorURL, "https://mediator.example.com/?"},
		{EnvMediatorURL, "https://mediator.example.com#"},
		{EnvMediatorURL, "https://mediator.example.com#frag"},
		{EnvRunnerGroupID, "0"},
		{EnvRunnerGroupID, "default"},
		{EnvRoutingVariableName, "1VAR"},
		{EnvRoutingVariableName, "GITHUB_RUNNER"},
		{EnvBuildSAName, "sa"},
		{EnvExecSAName, "cr-runner-mediator-sa"}, // duplicates the mediator SA
		{EnvAppPrivateKeySecretName, "bad/name"},
		{EnvWebhookSecretName, "cr-runner-gh-app-private-key"}, // duplicates the key secret
	}
	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			env := validEnv()
			env[tt.key] = tt.value
			_, err := Load(getenvFrom(env))
			if err == nil {
				t.Fatalf("Load accepted %s=%q", tt.key, tt.value)
			}
			// Duplicate-name errors are reported on whichever key sorts
			// last, so only require the key to appear in the message.
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("error does not name %s: %v", tt.key, err)
			}
		})
	}
}

func TestLoadMemoryMustFitCPU(t *testing.T) {
	// Cloud Run Jobs: 4 CPU allows at most 16 GiB; 6 CPU at most 24 GiB.
	env := validEnv()
	env[EnvRunnerCPU] = "4"
	env[EnvRunnerMemory] = "24Gi"
	if _, err := Load(getenvFrom(env)); err == nil || !strings.Contains(err.Error(), EnvRunnerMemory) {
		t.Errorf("4 CPU with 24Gi: err = %v, want %s error", err, EnvRunnerMemory)
	}
	env[EnvRunnerCPU] = "6"
	if _, err := Load(getenvFrom(env)); err != nil {
		t.Errorf("6 CPU with 24Gi should load: %v", err)
	}
}

// TestExampleEnvMatchesContract keeps cr-ci/config.example.env and the loader
// in lockstep: same keys, every key documented by a comment, the example
// defaults equal to the loader defaults, and no value set for a required key.
func TestExampleEnvMatchesContract(t *testing.T) {
	f, err := os.Open("../config.example.env")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	example := map[string]string{}
	var order []string
	commented := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
			commented = false
		case strings.HasPrefix(line, "#"):
			commented = true
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				t.Fatalf("malformed line %q", line)
			}
			if _, dup := example[k]; dup {
				t.Errorf("%s defined twice", k)
			}
			if !commented {
				t.Errorf("%s has no comment directly above it", k)
			}
			example[k] = v
			order = append(order, k)
			commented = false
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	sort.Strings(order)
	if !reflect.DeepEqual(order, Keys()) {
		t.Fatalf("config.example.env keys\n %v\ndiffer from loader keys\n %v", order, Keys())
	}
	for k, def := range Defaults() {
		if example[k] != def {
			t.Errorf("%s: example value %q, loader default %q", k, example[k], def)
		}
	}
}
