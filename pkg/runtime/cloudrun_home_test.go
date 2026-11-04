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

//go:build unix

package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

// Canaries planted in the broker-side home at every path the home copy
// must leave out. None may appear anywhere on the home leaf. They are
// obviously fake values.
const (
	crCanaryToken        = "fake-canary-7242-token-not-a-credential"
	crCanarySecretsJSON  = "fake-canary-7242-secrets-json-not-a-credential"
	crCanaryFileSecret   = "fake-canary-7242-file-secret-not-a-credential"
	crCanaryAuthFile     = "fake-canary-7242-auth-file-not-a-credential"
	crCanaryHarness      = "fake-canary-7242-harness-secret-not-a-credential"
	crCanaryResolvedAuth = "fake-canary-7242-resolved-auth-output-not-a-credential"
)

var crHomeCanaries = []string{crCanaryToken, crCanarySecretsJSON, crCanaryFileSecret, crCanaryAuthFile, crCanaryHarness, crCanaryResolvedAuth}

// crHomeEntry is a regular file of a broker-side home fixture.
type crHomeEntry struct {
	content string
	mode    os.FileMode
}

// crHomeFixtureFiles are the copied files of the fixture with the mode
// each must have on the leaf (the source mode capped: 0755 for an
// executable, 0644 otherwise, no setuid, setgid or sticky).
var crHomeFixtureFiles = map[string]struct {
	src  crHomeEntry
	want os.FileMode
}{
	".bashrc":                       {crHomeEntry{"bashrc", 0o644}, 0o644},
	".profile":                      {crHomeEntry{"profile", 0o600}, 0o600},
	"bin/tool":                      {crHomeEntry{"#!/bin/sh\n", 0o755 | os.ModeSetuid}, 0o755},
	"bin/sticky":                    {crHomeEntry{"#!/bin/sh\n", 0o775 | os.ModeSetgid | os.ModeSticky}, 0o755},
	".scion/agent-info.json":        {crHomeEntry{"{}", 0o664}, 0o644},
	".scion/hooks/pre-start.d/10":   {crHomeEntry{"#!/bin/sh\n", 0o775}, 0o755},
	".scion/harness/manifest.json":  {crHomeEntry{"{}", 0o644}, 0o644},
	".claude/settings.json":         {crHomeEntry{"{}", 0o644}, 0o644},
	".config/gcloud/configurations": {crHomeEntry{"cfg", 0o644}, 0o644},
}

// crHomeFixtureExcluded are the fixture files the copy must leave out, each
// holding a canary.
var crHomeFixtureExcluded = map[string]string{
	cloudRunHubTokenRel:                            crCanaryToken,
	cloudRunHubTokenTmpHomeRel:                     crCanaryToken,
	".scion/.scion-token.tmp.tmp-0123456789abcdef": crCanaryToken,
	cloudRunSecretsJSONHomeRel:                     crCanarySecretsJSON,
	".config/gcloud/creds.json":                    crCanaryFileSecret,
	".claude/.credentials.json":                    crCanaryAuthFile,
	harnessBundleSecretsRel + "/API_KEY":           crCanaryHarness,
	harnessBundleOutputsRel + "/resolved-env":      crCanaryResolvedAuth,
}

// writeCRHomeFixture builds a broker-side agent home with the copied
// files, the excluded canary files, a link and a FIFO.
func writeCRHomeFixture(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "agent-home")
	write := func(rel, content string, mode os.FileMode) {
		p := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	for rel, f := range crHomeFixtureFiles {
		write(rel, f.src.content, f.src.mode)
	}
	for rel, canary := range crHomeFixtureExcluded {
		write(rel, canary+"\n", 0o600)
	}
	if err := os.Symlink(".bashrc", filepath.Join(home, ".vimrc")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(home, ".scion", "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// crHomeFixtureSecrets are the resolved secrets and auth whose home
// targets are in crHomeFixtureExcluded.
func crHomeFixtureSecrets(cfg *RunConfig) {
	cfg.ResolvedSecrets = []api.ResolvedSecret{
		{Name: "creds", Type: "file", Target: "~/.config/gcloud/creds.json", Value: "c2VjcmV0"},
		{Name: "region", Type: "variable", Target: "EXAMPLE_REGION", Value: "us"},
	}
	cfg.ResolvedAuth = &api.ResolvedAuth{Files: []api.FileMapping{{SourcePath: "/nonexistent/creds", ContainerPath: "~/.claude/.credentials.json"}}}
}

// crHomeRun is a Cloud Run start on the REAL runtime path: the real
// CloudRunRuntime.Run and provisionCloudRunNFS on a temp share (only the
// NFS filesystem check stubbed), the fake Instances client, and the home
// fixture as the broker-side agent home. The agent is clone-per-agent so
// the provisioning needs no project checkout. It returns the runtime, the
// config and the agent's home leaf on the share.
func crHomeRun(t *testing.T, fake *fakeInstancesClient) (*CloudRunRuntime, RunConfig, string) {
	t.Helper()
	rt, cfg, agentDir := rollbackTestRun(t, fake, true)
	cfg.UnixUsername = "scion"
	cfg.NFSUID, cfg.NFSGID = os.Getuid(), os.Getgid()
	cfg.HomeDir = writeCRHomeFixture(t)
	crHomeFixtureSecrets(&cfg)
	return rt, cfg, filepath.Join(agentDir, "home-"+crTestAgentID)
}

// crLeafTree walks the home leaf without following links and returns its
// regular files (content and mode), its directories, and any other entry.
func crLeafTree(t *testing.T, leaf string) (files map[string]crHomeEntry, dirs, other []string) {
	t.Helper()
	files = map[string]crHomeEntry{}
	err := filepath.WalkDir(leaf, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == leaf {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, leaf+string(filepath.Separator)))
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			dirs = append(dirs, rel)
		case fi.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			files[rel] = crHomeEntry{string(data), fi.Mode()}
		default:
			other = append(other, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk home leaf: %v", err)
	}
	return files, dirs, other
}

// assertNoCanaryUnder fails when any canary occurs in any file below dir.
func assertNoCanaryUnder(t *testing.T, dir string) {
	t.Helper()
	files, _, _ := crLeafTree(t, dir)
	for rel, f := range files {
		for _, c := range crHomeCanaries {
			if strings.Contains(f.content, c) {
				t.Errorf("home leaf file %s carries an excluded secret", rel)
			}
		}
	}
}

// The home leaf after a start holds exactly the home transfer set, file by
// file (content, capped mode, owner), with every excluded path absent and
// no canary anywhere on the leaf; links and special files are not copied.
func TestCloudRunRun_HomeLeafEqualsTransferSet(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt, cfg, leaf := crHomeRun(t, fake)
	logs := captureRuntimeLog(t)

	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	files, dirs, other := crLeafTree(t, leaf)
	if len(other) != 0 {
		t.Errorf("home leaf has links or special files: %v", other)
	}
	for rel, f := range crHomeFixtureFiles {
		got, ok := files[rel]
		switch {
		case !ok:
			t.Errorf("%s not on the home leaf", rel)
		case got.content != f.src.content:
			t.Errorf("%s content differs on the home leaf", rel)
		case got.mode != f.want:
			t.Errorf("%s mode = %v on the home leaf, want %v", rel, got.mode, f.want)
		}
	}
	for rel := range files {
		if _, ok := crHomeFixtureFiles[rel]; !ok {
			t.Errorf("%s is on the home leaf but not in the home transfer set", rel)
		}
	}
	uid, gid := cloudRunOwnerIDs(cfg)
	for rel := range files {
		fi, err := os.Lstat(filepath.Join(leaf, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if int(st.Uid) != uid || int(st.Gid) != gid {
			t.Errorf("%s owned by %d:%d, want %d:%d", rel, st.Uid, st.Gid, uid, gid)
		}
	}
	for _, d := range []string{"bin", ".scion/hooks/pre-start.d", ".scion/harness"} {
		fi, err := os.Lstat(filepath.Join(leaf, filepath.FromSlash(d)))
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("directory %s on the home leaf: %v, err %v; want a 0700 directory", d, fi, err)
		}
	}
	for _, d := range dirs {
		if d == harnessBundleSecretsRel || d == harnessBundleOutputsRel {
			t.Errorf("%s exists on the home leaf", d)
		}
	}
	assertNoCanaryUnder(t, leaf)
	out := logs.String()
	for _, c := range crHomeCanaries {
		if strings.Contains(out, c) {
			t.Error("runtime log carries an excluded secret")
		}
	}
	if !strings.Contains(out, "agent home provisioned onto the share") || !strings.Contains(out, "entry=.vimrc") {
		t.Errorf("no summary or skipped-entry log line (log length %d)", len(out))
	}
	if strings.Contains(out, leaf) {
		t.Error("runtime log names the host path of the home leaf")
	}
}

// The harness secrets reach the instance only inside the staged blob's
// dedicated field, never as an env value of their own or in argv; the
// instance spec carries the in-memory volume and both directory env vars.
func TestCloudRunRun_HarnessSecretsOnlyInBlobAndMemory(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt, cfg, _ := crHomeRun(t, fake)
	logs := captureRuntimeLog(t)

	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	c := crCreatedContainer(t, fake)
	var blob string
	seen := map[string]int{}
	for _, ev := range c.Env {
		seen[ev.Name]++
		v, _ := ev.GetValues().(*runpb.EnvVar_Value)
		if v == nil {
			continue
		}
		for _, canary := range crHomeCanaries {
			if strings.Contains(v.Value, canary) {
				t.Errorf("env var %s carries a harness, auth or other secret value from the home", ev.Name)
			}
		}
		if ev.Name == stagedsecrets.EnvVar {
			blob = v.Value
		}
	}
	for i, a := range append(append([]string{}, c.Command...), c.Args...) {
		for _, canary := range crHomeCanaries {
			if strings.Contains(a, canary) {
				t.Errorf("command/arg %d carries a harness, auth or other secret value from the home", i)
			}
		}
	}
	staged, err := stagedsecrets.Decode(blob)
	if err != nil {
		t.Fatalf("decode staged blob: %v", err)
	}
	if len(staged.HarnessSecrets) != 1 || staged.HarnessSecrets[0].Name != "API_KEY" {
		t.Fatalf("staged harness secrets = %d entries, want API_KEY only", len(staged.HarnessSecrets))
	}
	if v, _ := base64.StdEncoding.DecodeString(staged.HarnessSecrets[0].Value); string(v) != crCanaryHarness+"\n" {
		t.Error("staged harness secret content differs from the home's")
	}
	for _, fsec := range staged.FileSecrets {
		if strings.Contains(fsec.Target, "harness") {
			t.Errorf("harness secret staged as a file secret (target %s)", fsec.Target)
		}
	}

	env := crInstanceEnv(t, fake)
	if env[harnessSecretsDirEnvVar] != "/run/scion/mem/harness-secrets" || env[harnessOutputsDirEnvVar] != "/run/scion/mem/outputs" {
		t.Errorf("harness dir env = %q, %q; want the in-memory directories", env[harnessSecretsDirEnvVar], env[harnessOutputsDirEnvVar])
	}
	for _, name := range []string{harnessSecretsDirEnvVar, harnessOutputsDirEnvVar} {
		if seen[name] != 1 {
			t.Errorf("env %s set %d times, want 1", name, seen[name])
		}
	}
	inst := fake.createReqs[0].Instance
	var mem *runpb.Volume
	for _, v := range inst.Volumes {
		if v.Name == cloudRunMemVolume {
			mem = v
		}
	}
	if mem == nil || mem.GetEmptyDir() == nil || mem.GetEmptyDir().GetMedium() != runpb.EmptyDirVolumeSource_MEMORY || mem.GetEmptyDir().GetSizeLimit() != "16Mi" {
		t.Fatalf("instance has no 16Mi in-memory volume: %v", mem)
	}
	mounted := false
	for _, m := range c.VolumeMounts {
		if m.Name == cloudRunMemVolume && m.MountPath == "/run/scion/mem" {
			mounted = true
		}
	}
	if !mounted {
		t.Error("in-memory volume not mounted at /run/scion/mem")
	}
	out := logs.String()
	if strings.Contains(out, crCanaryHarness) {
		t.Error("runtime log carries a harness secret value")
	}
	if !strings.Contains(out, "harness_secrets_bytes=") || !strings.Contains(out, "total_bytes=") {
		t.Errorf("no staged-secrets size log line (log length %d)", len(out))
	}
}

// An env secret or cfg.Env entry cannot point the harness directories
// elsewhere: the runtime's own values are the only ones in the spec.
func TestCloudRunRun_HarnessDirEnvNotOverridable(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt, cfg, _ := crHomeRun(t, fake)
	cfg.Env = append(cfg.Env, harnessSecretsDirEnvVar+"=/home/scion/elsewhere")
	cfg.ResolvedSecrets = append(cfg.ResolvedSecrets, api.ResolvedSecret{Name: "o", Type: "environment", Target: harnessOutputsDirEnvVar, Value: "/home/scion/out"})
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := map[string][]string{}
	for _, ev := range crCreatedContainer(t, fake).Env {
		if v, ok := ev.GetValues().(*runpb.EnvVar_Value); ok {
			seen[ev.Name] = append(seen[ev.Name], v.Value)
		}
	}
	if got := seen[harnessSecretsDirEnvVar]; !slices.Equal(got, []string{k8sHarnessSecretsDir}) {
		t.Errorf("%s = %v, want only %s", harnessSecretsDirEnvVar, got, k8sHarnessSecretsDir)
	}
	if got := seen[harnessOutputsDirEnvVar]; !slices.Equal(got, []string{k8sHarnessOutputsDir}) {
		t.Errorf("%s = %v, want only %s", harnessOutputsDirEnvVar, got, k8sHarnessOutputsDir)
	}
}

// A link planted on the home leaf where the copy writes a file or a
// directory is replaced, never followed or written through: the start
// succeeds, the entry becomes the provisioned file or a real 0700
// directory, and the link target outside the leaf (a file, or a directory
// holding files) is unchanged. Replacements are logged as a count only.
func TestCloudRunRun_PlantedLeafLinkReplaced(t *testing.T) {
	for _, tc := range []struct {
		name string
		rel  string // the link on the leaf
		dir  bool   // the link points to a directory
	}{
		{"file", ".bashrc", false},
		{"directory", "bin", true},
		{"nested directory", ".scion/hooks", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			rt, cfg, leaf := crHomeRun(t, fake)
			logs := captureRuntimeLog(t)
			outside := filepath.Join(t.TempDir(), "outside")
			keep := outside
			if tc.dir {
				keep = filepath.Join(outside, "keep")
				if err := os.MkdirAll(outside, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(keep, []byte("outside"), 0o644); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(leaf, filepath.FromSlash(tc.rel))
			if err := os.MkdirAll(filepath.Dir(link), 0o770); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}

			if _, err := rt.Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run: %v", err)
			}
			fi, err := os.Lstat(link)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case fi.Mode()&os.ModeSymlink != 0:
				t.Error("the planted link is still on the leaf")
			case tc.dir && (!fi.IsDir() || fi.Mode().Perm() != 0o700):
				t.Errorf("%s is %v, want a 0700 directory", tc.rel, fi.Mode())
			case !tc.dir && !fi.Mode().IsRegular():
				t.Errorf("%s is %v, want a regular file", tc.rel, fi.Mode())
			}
			if !tc.dir {
				if data, _ := os.ReadFile(link); string(data) != crHomeFixtureFiles[tc.rel].src.content {
					t.Error("the replaced file does not hold the provisioned content")
				}
			}
			if data, err := os.ReadFile(keep); err != nil || string(data) != "outside" {
				t.Errorf("the link target outside the leaf changed (err %v)", err)
			}
			if tc.dir {
				entries, _ := os.ReadDir(outside)
				if len(entries) != 1 || entries[0].Name() != "keep" {
					t.Errorf("the outside directory has %d entries, want only keep: the copy wrote through the link", len(entries))
				}
			}
			out := logs.String()
			if !strings.Contains(out, "replaced_links=1") {
				t.Errorf("no replaced-link count in the summary log (log length %d)", len(out))
			}
			if strings.Contains(out, outside) {
				t.Error("the log names the link target")
			}
		})
	}
}

// Any other entry of the wrong type on the leaf where the copy writes (a
// real directory where a file goes, a file where a directory goes, a FIFO)
// refuses the start with the fixed conflict error; nothing is removed and
// no instance is created.
func TestCloudRunRun_WrongTypeLeafEntryRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rel   string
		plant func(p string) error
	}{
		{"directory where a file goes", ".bashrc", func(p string) error { return os.Mkdir(p, 0o700) }},
		{"file where a directory goes", "bin", func(p string) error { return os.WriteFile(p, []byte("mine"), 0o600) }},
		{"fifo where a file goes", ".profile", func(p string) error { return syscall.Mkfifo(p, 0o600) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			rt, cfg, leaf := crHomeRun(t, fake)
			if err := os.MkdirAll(leaf, 0o770); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(leaf, filepath.FromSlash(tc.rel))
			if err := tc.plant(p); err != nil {
				t.Fatal(err)
			}
			before, _ := os.Lstat(p)
			_, err := rt.Run(context.Background(), cfg)
			if !errors.Is(err, errCloudRunHomeConflict) {
				t.Fatalf("Run = %v, want %v", err, errCloudRunHomeConflict)
			}
			after, err := os.Lstat(p)
			if err != nil || after.Mode() != before.Mode() {
				t.Errorf("the wrong-type entry was changed or removed (err %v)", err)
			}
			if len(fake.createReqs) != 0 {
				t.Error("an instance was created after the home copy was refused")
			}
		})
	}
}

// A start that finds the agent's instance held by another run refuses with
// ErrRunConflict before the home is touched: the leaf, including a file in
// the transfer set and planted harness outputs, is byte-identical after.
func TestCloudRunRun_RunConflictLeavesLeafUnchanged(t *testing.T) {
	other := &runpb.Instance{Name: "x", Labels: map[string]string{sanitizeGCPLabelKey(api.LabelRunID): "run-other"}}
	fake := &fakeInstancesClient{getInstance: other}
	rt, cfg, leaf := crHomeRun(t, fake)
	files := map[string]string{".bashrc": "the live instance's own bashrc", harnessBundleOutputsRel + "/x": "live output"}
	for rel, content := range files {
		p := filepath.Join(leaf, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	beforeFiles, beforeDirs, _ := crLeafTree(t, leaf)

	_, err := rt.Run(context.Background(), cfg)
	if !errors.Is(err, ErrRunConflict) {
		t.Fatalf("Run = %v, want ErrRunConflict", err)
	}
	afterFiles, afterDirs, _ := crLeafTree(t, leaf)
	if len(afterFiles) != len(beforeFiles) || !slices.Equal(beforeDirs, afterDirs) {
		t.Errorf("the leaf changed: %d files / %v dirs before, %d / %v after", len(beforeFiles), beforeDirs, len(afterFiles), afterDirs)
	}
	for rel, content := range files {
		if afterFiles[rel].content != content {
			t.Errorf("%s on the leaf changed", rel)
		}
	}
}

// After the copy, harness secrets and outputs left on the home leaf are
// removed at each start; a link planted there is removed itself and never
// followed, and a link at a parent component refuses the start, with
// nothing outside the leaf touched either way.
func TestCloudRunRun_RemovesHarnessMaterialFromLeaf(t *testing.T) {
	t.Run("stale files", func(t *testing.T) {
		fake := &fakeInstancesClient{getErr: notFoundErr()}
		rt, cfg, leaf := crHomeRun(t, fake)
		for _, rel := range []string{harnessBundleSecretsRel + "/OLD", harnessBundleOutputsRel + "/old-env"} {
			p := filepath.Join(leaf, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(crCanaryHarness), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := rt.Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run: %v", err)
		}
		for _, rel := range []string{harnessBundleSecretsRel, harnessBundleOutputsRel} {
			if _, err := os.Lstat(filepath.Join(leaf, filepath.FromSlash(rel))); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s still on the home leaf (err %v)", rel, err)
			}
		}
		assertNoCanaryUnder(t, leaf)
	})
	for _, rel := range []string{harnessBundleSecretsRel, harnessBundleOutputsRel} {
		t.Run("link at "+rel, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			rt, cfg, leaf := crHomeRun(t, fake)
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.MkdirAll(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			keep := filepath.Join(outside, "keep")
			if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(leaf, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			if _, err := rt.Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if _, err := os.Lstat(link); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the link at %s is still there (err %v)", rel, err)
			}
			if data, err := os.ReadFile(keep); err != nil || string(data) != "keep" {
				t.Errorf("the removal followed the link: %s outside the leaf is gone or changed (err %v)", keep, err)
			}
		})
	}
	t.Run("link at a parent component", func(t *testing.T) {
		fake := &fakeInstancesClient{getErr: notFoundErr()}
		rt, cfg, leaf := crHomeRun(t, fake)
		// No .scion/harness in the source, so only the removal reaches the
		// planted parent link.
		if err := os.RemoveAll(filepath.Join(cfg.HomeDir, ".scion", "harness")); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside")
		keep := filepath.Join(outside, "secrets", "keep")
		if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(leaf, ".scion"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(leaf, ".scion", "harness")); err != nil {
			t.Fatal(err)
		}
		_, err := rt.Run(context.Background(), cfg)
		if !errors.Is(err, errCloudRunHomeConflict) {
			t.Fatalf("Run = %v, want %v", err, errCloudRunHomeConflict)
		}
		if data, err := os.ReadFile(keep); err != nil || string(data) != "keep" {
			t.Errorf("the removal followed a parent link: %s is gone or changed (err %v)", keep, err)
		}
		if len(fake.createReqs) != 0 {
			t.Error("an instance was created after the removal was refused")
		}
	})
}

// Over the entry or depth bound the start fails closed with the fixed
// message, before any instance is created.
func TestCloudRunRun_HomeCopyLimits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, home string)
	}{
		{"entries", func(t *testing.T, home string) {
			dir := filepath.Join(home, "many")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for i := 0; i <= cloudRunHomeMaxEntries; i++ {
				if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(i)), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"depth", func(t *testing.T, home string) {
			p := home
			for i := 0; i <= cloudRunHomeMaxDepth; i++ {
				p = filepath.Join(p, "d")
			}
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			rt, cfg, _ := crHomeRun(t, fake)
			tc.build(t, cfg.HomeDir)
			_, err := rt.Run(context.Background(), cfg)
			if !errors.Is(err, errCloudRunHomeTooLarge) {
				t.Fatalf("Run = %v, want %v", err, errCloudRunHomeTooLarge)
			}
			if len(fake.createReqs) != 0 {
				t.Error("an instance was created over the copy limits")
			}
		})
	}
}

// A start without a broker-side home copies nothing and still builds the
// in-memory volume (the harness outputs go there).
func TestCloudRunRun_NoHomeDirCopiesNothing(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt, cfg, leaf := crHomeRun(t, fake)
	cfg.HomeDir = ""
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if entries, err := os.ReadDir(leaf); err != nil || len(entries) != 0 {
		t.Errorf("home leaf has %d entries (err %v), want none", len(entries), err)
	}
}

// An oversized blob fails the start with the fixed error naming only the
// largest field class, never a secret name, key, path or value.
func TestCloudRunRun_OversizedBlobNamesFieldClassOnly(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt, cfg, _ := crHomeRun(t, fake)
	big := filepath.Join(cfg.HomeDir, filepath.FromSlash(harnessBundleSecretsRel), "BIG_HARNESS_KEY")
	if err := os.WriteFile(big, bytes.Repeat([]byte("h"), cloudRunMaxEnvValueBytes*3/4), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := rt.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run accepted an oversized staged blob")
	}
	msg := err.Error()
	if !strings.Contains(msg, "the largest part is the "+stagedClassHarnessSecrets) {
		t.Errorf("error does not name the harness secrets class: %q", msg)
	}
	for _, leak := range []string{"BIG_HARNESS_KEY", "API_KEY", "creds", "hhhh", cfg.HomeDir} {
		if strings.Contains(msg, leak) {
			t.Errorf("error names %q", leak)
		}
	}
	if len(fake.createReqs) != 0 {
		t.Error("an instance was created with an oversized blob")
	}
}

func TestStagedSizesLargestClass(t *testing.T) {
	for _, tc := range []struct {
		s    stagedSizes
		want string
	}{
		{stagedSizes{HubToken: 3, FileSecrets: 2, HarnessSecrets: 1}, stagedClassHubToken},
		{stagedSizes{HubToken: 1, FileSecrets: 3, HarnessSecrets: 2}, stagedClassFileSecrets},
		{stagedSizes{HubToken: 1, FileSecrets: 2, HarnessSecrets: 3}, stagedClassHarnessSecrets},
	} {
		if got := tc.s.largestClass(); got != tc.want {
			t.Errorf("%+v.largestClass() = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// Without harness secrets, the docker/podman payload (prepareContainerSecretEnv)
// carries no harness_secrets field.
func TestStagedPayloadHasNoHarnessSecretsField(t *testing.T) {
	cfg := RunConfig{UnixUsername: "scion", ResolvedSecrets: []api.ResolvedSecret{{Name: "r", Type: "variable", Target: "R", Value: "v"}}}
	if err := prepareContainerSecretEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	for _, e := range cfg.Env {
		k, v, _ := strings.Cut(e, "=")
		if k != stagedsecrets.EnvVar {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "harness_secrets") {
			t.Error("payload carries a harness_secrets field")
		}
	}
}

func TestCloudRunHomesWarningFor(t *testing.T) {
	withHomes := &config.V1ServerConfig{ProjectStorage: &config.V1ProjectStorageConfig{Backend: "local", Homes: "local"}}
	without := &config.V1ServerConfig{ProjectStorage: &config.V1ProjectStorageConfig{Backend: "local"}}
	for _, tc := range []struct {
		rt   string
		srv  *config.V1ServerConfig
		want string
	}{
		{"cloudrun", withHomes, CloudRunHomesWarning},
		{"cloudrun", without, ""},
		{"cloudrun", nil, ""},
		{"kubernetes", withHomes, ""},
		{"docker", withHomes, ""},
	} {
		if got := CloudRunHomesWarningFor(tc.rt, tc.srv); got != tc.want {
			t.Errorf("CloudRunHomesWarningFor(%q) = %q, want %q", tc.rt, got, tc.want)
		}
	}
	if strings.ContainsAny(CloudRunHomesWarning, "/~") {
		t.Error("the warning names a path")
	}
}

// scion doctor shows the warning as a warn check for the cloudrun runtime.
func TestCloudRunDoctor_HomesWarning(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)
	rt.testActAs = func(context.Context, string) (bool, error) { return true, nil }
	srv := &config.V1ServerConfig{ProjectStorage: &config.V1ProjectStorageConfig{Backend: "local", Homes: "local"}}
	var found bool
	for _, c := range rt.RunDiagnostics(DiagnosticOpts{Server: srv}).Checks {
		if c.Message == CloudRunHomesWarning {
			found = c.Status == "warn"
		}
	}
	if !found {
		t.Error("doctor did not report the homes warning as warn")
	}
	for _, c := range rt.RunDiagnostics(DiagnosticOpts{}).Checks {
		if c.Message == CloudRunHomesWarning {
			t.Error("doctor reported the homes warning without the setting")
		}
	}
}

// A linked .scion directory in the broker-side home is never followed for
// the harness secrets: nothing is staged, no value reaches the instance or
// the log, and the start continues with a warning, as for the hub
// credential (TestCloudRunRun_LinkedHubTokenRefused).
func TestCloudRunRun_LinkedHarnessSecretsDirNotStaged(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)
	logs := captureRuntimeLog(t)

	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, "harness", "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "harness", "secrets", "API_KEY"), []byte(crCanaryHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "agent-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(home, ".scion")); err != nil {
		t.Fatal(err)
	}

	cfg := runConfigForTest()
	cfg.UnixUsername = "scion"
	cfg.HomeDir = home
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, ev := range crCreatedContainer(t, fake).Env {
		v, _ := ev.GetValues().(*runpb.EnvVar_Value)
		if v != nil && strings.Contains(v.Value, crCanaryHarness) {
			t.Errorf("env var %s carries a harness secret value", ev.Name)
		}
		if ev.Name == stagedsecrets.EnvVar {
			t.Errorf("%s set: harness secrets behind a linked .scion were staged", stagedsecrets.EnvVar)
		}
	}
	out := logs.String()
	assertHarnessNotStagedWarning(t, out, harnessSecretsCondLinked)
	if strings.Contains(out, crCanaryHarness) {
		t.Error("log carries a harness secret value")
	}
}

// assertHarnessNotStagedWarning checks that the "not staged" warning was
// logged with the condition cond, and names no host path or entry.
func assertHarnessNotStagedWarning(t *testing.T, out, cond string) {
	t.Helper()
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "harness secrets are not staged") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no warning that the harness secrets are not staged (log length %d)", len(out))
	}
	if !strings.Contains(line, "condition="+cond) {
		t.Errorf("warning does not carry condition=%s", cond)
	}
	if strings.Contains(line, "entry=") || strings.Contains(line, "/tmp/") {
		t.Error("warning names an entry or a host path")
	}
}

// A regular file at .scion/harness/secrets in the broker-side home stages
// no harness secrets; the start continues with a not_directory warning.
func TestCloudRunRun_HarnessSecretsPathNotDirectoryNotStaged(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt, cfg, _ := crHomeRun(t, fake)
	logs := captureRuntimeLog(t)
	secrets := filepath.Join(cfg.HomeDir, filepath.FromSlash(harnessBundleSecretsRel))
	if err := os.RemoveAll(secrets); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secrets, []byte(crCanaryHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	env := crInstanceEnv(t, fake)
	if blob, ok := env[stagedsecrets.EnvVar]; ok {
		staged, err := stagedsecrets.Decode(blob)
		if err != nil {
			t.Fatalf("decode staged blob: %v", err)
		}
		if len(staged.HarnessSecrets) != 0 {
			t.Errorf("%d harness secrets staged from a non-directory secrets path", len(staged.HarnessSecrets))
		}
	}
	for k, v := range env {
		if strings.Contains(v, crCanaryHarness) {
			t.Errorf("env var %s carries the harness canary", k)
		}
	}
	assertHarnessNotStagedWarning(t, logs.String(), harnessSecretsCondNotDirectory)
}

// A regular file at .scion or .scion/harness in the broker-side home
// refuses the start with the fixed conflict error in the harness secrets
// reader, BEFORE any provisioning: no agent directory or home leaf is
// created on the share and no instance is created or started. The log line
// is the "start is refused" one with condition=not_directory, and no
// provision-and-rollback is logged.
func TestCloudRunRun_HarnessParentNotDirectoryRefused(t *testing.T) {
	for _, tc := range []struct{ name, rel string }{{"dot_scion", ".scion"}, {"dot_scion_harness", ".scion/harness"}} {
		rel := tc.rel
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			rt, cfg, leaf := crHomeRun(t, fake)
			logs := captureRuntimeLog(t)
			p := filepath.Join(cfg.HomeDir, filepath.FromSlash(rel))
			if err := os.RemoveAll(p); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := rt.Run(context.Background(), cfg)
			if !errors.Is(err, errCloudRunHomeConflict) {
				t.Fatalf("Run = %v, want %v", err, errCloudRunHomeConflict)
			}
			for _, d := range []string{leaf, filepath.Dir(leaf)} {
				if _, err := os.Lstat(d); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s exists (err %v): the refusal came after provisioning", d, err)
				}
			}
			if len(fake.createReqs) != 0 || len(fake.startReqs) != 0 || len(fake.getReqs) != 0 {
				t.Errorf("%d get, %d create and %d start requests, want none", len(fake.getReqs), len(fake.createReqs), len(fake.startReqs))
			}
			var line string
			for _, l := range strings.Split(logs.String(), "\n") {
				if strings.Contains(l, "harness secrets directory in the agent home is not a directory; the start is refused") {
					line = l
				}
			}
			if line == "" || !strings.Contains(line, "condition="+harnessSecretsCondNotDirectory) {
				t.Errorf("no refusal line with condition=%s (got %q)", harnessSecretsCondNotDirectory, line)
			}
			if strings.Contains(logs.String(), "harness secrets are not staged") {
				t.Error("the not-staged (continue) warning was logged for a refused start")
			}
			// No provisioning ran at all: a refusal placed after the NFS
			// provision with a rollback would leave no directories either, but
			// it logs the rollback.
			if strings.Contains(logs.String(), "removed the agent directories provisioned for a start whose instance was not created") {
				t.Error("the agent directories were provisioned and rolled back: the refusal came after provisioning")
			}
		})
	}
}

// This run's own existing instance gets the home before it is started: the
// leaf holds exactly the transfer set, planted harness outputs are removed,
// one StartInstance and no create. With the start failing, the leaf is
// still provisioned (the copy ran first).
func TestCloudRunRun_OwnExistingInstanceGetsHome(t *testing.T) {
	for _, startFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("startFails=%v", startFails), func(t *testing.T) {
			own := &runpb.Instance{Name: "x", Labels: map[string]string{sanitizeGCPLabelKey(api.LabelRunID): "run-1"}}
			fake := &fakeInstancesClient{getInstance: own}
			if startFails {
				fake.startErr = errors.New("start refused")
			}
			rt, cfg, leaf := crHomeRun(t, fake)
			out := filepath.Join(leaf, filepath.FromSlash(harnessBundleOutputsRel), "x")
			if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(out, []byte("old output"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := rt.Run(context.Background(), cfg)
			if startFails != (err != nil) {
				t.Fatalf("Run = %v, want error=%v", err, startFails)
			}
			if len(fake.startReqs) != 1 || len(fake.createReqs) != 0 {
				t.Errorf("%d start and %d create requests, want 1 and 0", len(fake.startReqs), len(fake.createReqs))
			}
			if _, err := os.Lstat(out); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("planted harness output still on the leaf (err %v)", err)
			}
			files, _, _ := crLeafTree(t, leaf)
			for rel := range crHomeFixtureFiles {
				if _, ok := files[rel]; !ok {
					t.Errorf("%s not on the leaf", rel)
				}
			}
			for rel := range files {
				if _, ok := crHomeFixtureFiles[rel]; !ok {
					t.Errorf("%s on the leaf but not in the transfer set", rel)
				}
			}
		})
	}
}
