//go:build linux

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

package homeprep

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// These tests run as the invoking user: the home is a temporary directory
// owned by that user, standing in for the uid-1000 home on the export.

// bothResolvers runs f with openat2 and with the per-component fallback.
func bothResolvers(t *testing.T, f func(t *testing.T)) {
	for _, disabled := range []bool{false, true} {
		name := "openat2"
		if disabled {
			name = "walk"
		}
		t.Run(name, func(t *testing.T) {
			old := openat2Disabled
			openat2Disabled = disabled
			t.Cleanup(func() { openat2Disabled = old })
			f(t)
		})
	}
}

type prepEnv struct {
	t        *testing.T
	base     string
	home     string
	mem      string
	skeleton string
	logs     []string
}

func newPrepEnv(t *testing.T) *prepEnv {
	t.Helper()
	base := t.TempDir()
	e := &prepEnv{t: t, base: base, home: filepath.Join(base, "home"), mem: filepath.Join(base, "mem"), skeleton: filepath.Join(base, "image-home")}
	for _, d := range []string{e.home, e.mem, e.skeleton} {
		require.NoError(t, os.Mkdir(d, 0o755))
	}
	return e
}

func (e *prepEnv) opts(startID string, links ...Link) PrepareOptions {
	return PrepareOptions{
		Home: e.home, MemDir: e.mem, AgentID: testAgentID, StartID: startID,
		Links: links, SkeletonSource: e.skeleton, SkeletonMaxBytes: 1 << 20,
		Log: func(format string, args ...any) { e.logs = append(e.logs, format) },
	}
}

func (e *prepEnv) write(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.home, rel)
	require.NoError(e.t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(e.t, os.WriteFile(p, []byte(content), 0o644))
}

func (e *prepEnv) sentinel() *Sentinel {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.home, SentinelName))
	require.NoError(e.t, err)
	s, err := parseSentinel(data)
	require.NoError(e.t, err)
	return s
}

func (e *prepEnv) memJSON(name string, v any) {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.mem, name))
	require.NoError(e.t, err)
	require.NoError(e.t, json.Unmarshal(data, v))
}

func (e *prepEnv) homeNames() []string {
	e.t.Helper()
	ents, err := os.ReadDir(e.home)
	require.NoError(e.t, err)
	var out []string
	for _, d := range ents {
		out = append(out, d.Name())
	}
	sort.Strings(out)
	return out
}

func readlink(t *testing.T, p string) string {
	t.Helper()
	l, err := os.Readlink(p)
	require.NoError(t, err)
	return l
}

var credLink = Link{Target: ".config/gcloud/creds.json", Source: "/run/scion/agent-secrets/creds", Mode: "0600"}
var secretsLink = Link{Target: ".scion/secrets.json", Source: "/run/scion/agent-secrets/secrets.json", Mode: "0600"}

func TestPrepare_FirstSeedThenSeedOver(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		e := newPrepEnv(t)
		require.NoError(t, os.WriteFile(filepath.Join(e.skeleton, ".bashrc"), []byte("rc"), 0o640))
		require.NoError(t, os.MkdirAll(filepath.Join(e.skeleton, ".local", "bin"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(e.skeleton, ".local", "bin", "tool"), []byte("#!"), 0o755))
		require.NoError(t, os.Symlink(".bashrc", filepath.Join(e.skeleton, ".profile")))
		require.NoError(t, unix.Mkfifo(filepath.Join(e.skeleton, "fifo"), 0o644))

		mode, err := Prepare(e.opts("start-1", credLink, secretsLink))
		require.NoError(t, err)
		assert.Equal(t, ModeSeed, mode)

		s := e.sentinel()
		assert.Equal(t, StateSeeding, s.State)
		assert.Equal(t, "start-1", s.StartID)
		assert.Equal(t, testAgentID, s.AgentID)
		assert.Equal(t, os.Getuid(), s.UID)
		assert.Equal(t, SkeletonCopied, s.Skeleton)

		data, err := os.ReadFile(filepath.Join(e.home, ".bashrc"))
		require.NoError(t, err)
		assert.Equal(t, "rc", string(data))
		st, err := os.Stat(filepath.Join(e.home, ".local", "bin", "tool"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), st.Mode().Perm())
		assert.Equal(t, ".bashrc", readlink(t, filepath.Join(e.home, ".profile")))
		_, err = os.Lstat(filepath.Join(e.home, "fifo"))
		assert.True(t, os.IsNotExist(err), "special files are not copied")

		assert.Equal(t, credLink.Source, readlink(t, filepath.Join(e.home, credLink.Target)))
		assert.Equal(t, secretsLink.Source, readlink(t, filepath.Join(e.home, secretsLink.Target)))

		var mf ModeFile
		e.memJSON(ModeFileName, &mf)
		assert.Equal(t, ModeFile{Mode: ModeSeed, StartID: "start-1"}, mf)
		var res LinksResult
		e.memJSON(LinksResultFileName, &res)
		assert.Equal(t, []LinkResult{{credLink.Target, LinkLinked}, {secretsLink.Target, LinkLinked}}, res.Links)

		// The broker transfers the home, then marks it seeded.
		require.NoError(t, MarkSeeded(e.home, testAgentID, "start-1"))
		s = e.sentinel()
		assert.Equal(t, StateSeeded, s.State)
		assert.NotEmpty(t, s.SeededAt)

		// Second start: seed-over. Hooks are cleared; the image home is
		// not copied again.
		e.write(".scion/hooks/old-hook.sh", "echo")
		e.write(".scion/hooks/sub/x", "x")
		require.NoError(t, os.Remove(filepath.Join(e.home, ".bashrc")))
		mode, err = Prepare(e.opts("start-2", credLink, secretsLink))
		require.NoError(t, err)
		assert.Equal(t, ModeSeedOver, mode)
		ents, err := os.ReadDir(filepath.Join(e.home, ".scion", "hooks"))
		require.NoError(t, err)
		assert.Empty(t, ents)
		_, err = os.Stat(filepath.Join(e.home, ".bashrc"))
		assert.True(t, os.IsNotExist(err), "the skeleton is copied on seed only")
		s = e.sentinel()
		assert.Equal(t, StateSeeded, s.State)
		assert.Equal(t, "start-2", s.StartID)
		e.memJSON(ModeFileName, &mf)
		assert.Equal(t, ModeSeedOver, mf.Mode)
	})
}

func TestPrepare_LinkRules(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		e := newPrepEnv(t)
		oldLink := Link{Target: ".old/token", Source: "/run/scion/auth-files/token", Mode: "0600"}
		userLink := Link{Target: ".config/user.json", Source: "/run/scion/agent-secrets/user", Mode: "0600"}
		_, err := Prepare(e.opts("s1", credLink, secretsLink, oldLink))
		require.NoError(t, err)
		require.NoError(t, MarkSeeded(e.home, testAgentID, "s1"))

		// A tool replaced a placed link with a file; the user put a file
		// at a target scion never linked; the old link is no longer
		// requested; the secrets link was replaced by the user's own link.
		cred := filepath.Join(e.home, credLink.Target)
		require.NoError(t, os.Remove(cred))
		require.NoError(t, os.WriteFile(cred, []byte("rewritten"), 0o600))
		e.write(userLink.Target, "mine")

		_, err = Prepare(e.opts("s2", credLink, secretsLink, userLink))
		require.NoError(t, err)
		assert.Equal(t, credLink.Source, readlink(t, cred), "a file over a recorded link is replaced by the link")
		data, err := os.ReadFile(filepath.Join(e.home, userLink.Target))
		require.NoError(t, err)
		assert.Equal(t, "mine", string(data), "a user file at a never-linked target is kept")
		_, err = os.Lstat(filepath.Join(e.home, oldLink.Target))
		assert.True(t, os.IsNotExist(err), "a recorded link no longer requested is removed")
		assert.Equal(t, secretsLink.Source, readlink(t, filepath.Join(e.home, secretsLink.Target)))

		var res LinksResult
		e.memJSON(LinksResultFileName, &res)
		assert.Equal(t, []LinkResult{{credLink.Target, LinkLinked}, {secretsLink.Target, LinkLinked}, {userLink.Target, LinkSkipped}}, res.Links)

		// A user's own symbolic link at a target scion never linked is kept.
		e2 := newPrepEnv(t)
		e2.write(SentinelName, `{"version":1,"agent_id":"`+testAgentID+`","state":"seeded","uid":`+itoa(os.Getuid())+`,"start_id":"x"}`)
		require.NoError(t, os.MkdirAll(filepath.Join(e2.home, ".scion"), 0o755))
		require.NoError(t, os.Symlink("/elsewhere", filepath.Join(e2.home, secretsLink.Target)))
		_, err = Prepare(e2.opts("s3", secretsLink))
		require.NoError(t, err)
		assert.Equal(t, "/elsewhere", readlink(t, filepath.Join(e2.home, secretsLink.Target)))
		e2.memJSON(LinksResultFileName, &res)
		assert.Equal(t, []LinkResult{{secretsLink.Target, LinkSkipped}}, res.Links)

		// A directory at a target is kept and the link skipped.
		e3 := newPrepEnv(t)
		require.NoError(t, os.MkdirAll(filepath.Join(e3.home, credLink.Target), 0o755))
		e3.write(SentinelName, `{"version":1,"agent_id":"`+testAgentID+`","state":"seeded","uid":`+itoa(os.Getuid())+`,"start_id":"x"}`)
		_, err = Prepare(e3.opts("s4", credLink))
		require.NoError(t, err)
		st, err := os.Lstat(filepath.Join(e3.home, credLink.Target))
		require.NoError(t, err)
		assert.True(t, st.IsDir())
		e3.memJSON(LinksResultFileName, &res)
		assert.Equal(t, []LinkResult{{credLink.Target, LinkSkipped}}, res.Links)

		// Without the link record (lost), a link already pointing at the
		// staged file is recognised as scion's.
		require.NoError(t, os.Remove(filepath.Join(e.home, LinksRecordPath)))
		_, err = Prepare(e.opts("s5", credLink, secretsLink))
		require.NoError(t, err)
		e.memJSON(LinksResultFileName, &res)
		assert.Equal(t, []LinkResult{{credLink.Target, LinkLinked}, {secretsLink.Target, LinkLinked}}, res.Links)
	})
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// A symbolic link in a link's parent path is never followed, whether it
// points outside the home or inside it: the link is skipped, the start
// continues, and nothing is written through the symbolic link.
func TestPrepare_ConfinedBeneathHome(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		seeded := func(e *prepEnv) {
			e.write(SentinelName, `{"version":1,"agent_id":"`+testAgentID+`","state":"seeded","uid":`+itoa(os.Getuid())+`,"start_id":"x"}`)
		}
		// Outside the home.
		e := newPrepEnv(t)
		outside := filepath.Join(e.base, "x")
		require.NoError(t, os.Mkdir(outside, 0o755))
		seeded(e)
		require.NoError(t, os.Symlink("../x", filepath.Join(e.home, ".config")))
		_, err := Prepare(e.opts("s1", credLink))
		require.NoError(t, err)
		ents, err := os.ReadDir(outside)
		require.NoError(t, err)
		assert.Empty(t, ents, "nothing may be created through the link")
		var res LinksResult
		e.memJSON(LinksResultFileName, &res)
		assert.Equal(t, []LinkResult{{credLink.Target, LinkSkipped}}, res.Links)

		// Inside the home (a user's symbolic link to their own config dir).
		e2 := newPrepEnv(t)
		seeded(e2)
		require.NoError(t, os.Mkdir(filepath.Join(e2.home, ".xdg"), 0o755))
		require.NoError(t, os.Symlink(".xdg", filepath.Join(e2.home, ".config")))
		_, err = Prepare(e2.opts("s2", credLink))
		require.NoError(t, err)
		ents, err = os.ReadDir(filepath.Join(e2.home, ".xdg"))
		require.NoError(t, err)
		assert.Empty(t, ents, "a link pointing inside the home is not followed either")
		e2.memJSON(LinksResultFileName, &res)
		assert.Equal(t, []LinkResult{{credLink.Target, LinkSkipped}}, res.Links)

		// .scion as a symbolic link: hooks are not cleared and the record
		// is not written through it.
		e3 := newPrepEnv(t)
		seeded(e3)
		out3 := filepath.Join(e3.base, "y")
		require.NoError(t, os.MkdirAll(filepath.Join(out3, "hooks"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(out3, "hooks", "keep"), []byte("k"), 0o644))
		require.NoError(t, os.Symlink("../y", filepath.Join(e3.home, ".scion")))
		_, err = Prepare(e3.opts("s3", secretsLink))
		require.NoError(t, err)
		_, err = os.Stat(filepath.Join(out3, "hooks", "keep"))
		require.NoError(t, err, "files behind the link are untouched")
		_, err = os.Lstat(filepath.Join(out3, ".home-links.json"))
		assert.True(t, os.IsNotExist(err))
		_, err = os.Lstat(filepath.Join(out3, "secrets.json"))
		assert.True(t, os.IsNotExist(err))
	})
}

// An image home whose .config is a symbolic link (to .xdg) seeds, and every
// later start succeeds; links under .config are skipped.
func TestPrepare_ImageHomeWithSymlinkedConfig(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		e := newPrepEnv(t)
		require.NoError(t, os.Mkdir(filepath.Join(e.skeleton, ".xdg"), 0o755))
		require.NoError(t, os.Symlink(".xdg", filepath.Join(e.skeleton, ".config")))
		mode, err := Prepare(e.opts("s1", credLink, secretsLink))
		require.NoError(t, err)
		assert.Equal(t, ModeSeed, mode)
		require.NoError(t, MarkSeeded(e.home, testAgentID, "s1"))
		for _, start := range []string{"s2", "s3"} {
			mode, err = Prepare(e.opts(start, credLink, secretsLink))
			require.NoError(t, err)
			assert.Equal(t, ModeSeedOver, mode)
		}
		var res LinksResult
		e.memJSON(LinksResultFileName, &res)
		assert.Equal(t, []LinkResult{{credLink.Target, LinkSkipped}, {secretsLink.Target, LinkLinked}}, res.Links)
		ents, err := os.ReadDir(filepath.Join(e.home, ".xdg"))
		require.NoError(t, err)
		assert.Empty(t, ents)
	})
}

// The home itself must not be a symbolic link.
func TestPrepare_SymlinkedHomeRefused(t *testing.T) {
	e := newPrepEnv(t)
	link := filepath.Join(e.base, "home-link")
	require.NoError(t, os.Symlink(e.home, link))
	o := e.opts("s1")
	o.Home = link
	_, err := Prepare(o)
	var ce *ClassError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, ErrClassUnavailable, ce.Class)
	assert.Empty(t, e.homeNames(), "nothing is written through a linked home")
	require.Error(t, MarkSeeded(link, testAgentID, "s1"))
}

func TestPrepare_FailClosedSentinel(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		tests := []struct {
			name  string
			setup func(e *prepEnv)
			want  string
		}{
			{"content without sentinel", func(e *prepEnv) { e.write("notes.txt", "x") }, "content but no sentinel"},
			{"symlinked sentinel", func(e *prepEnv) {
				target := filepath.Join(e.base, "elsewhere.json")
				require.NoError(t, os.WriteFile(target, []byte(`{"version":1,"agent_id":"`+testAgentID+`","state":"seeded","uid":`+itoa(os.Getuid())+`}`), 0o644))
				require.NoError(t, os.Symlink(target, filepath.Join(e.home, SentinelName)))
			}, "cannot read the home sentinel"},
			{"bad json", func(e *prepEnv) { e.write(SentinelName, "{") }, "invalid sentinel"},
			{"unknown version", func(e *prepEnv) { e.write(SentinelName, `{"version":9,"state":"seeded"}`) }, "unknown sentinel version"},
			{"other agent", func(e *prepEnv) {
				e.write(SentinelName, `{"version":1,"agent_id":"11111111-2222-4333-8444-555555555555","state":"seeded","uid":`+itoa(os.Getuid())+`}`)
			}, "belongs to agent"},
			{"other uid", func(e *prepEnv) {
				e.write(SentinelName, `{"version":1,"agent_id":"`+testAgentID+`","state":"seeded","uid":`+itoa(os.Getuid()+1)+`}`)
			}, "seeded by uid"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				e := newPrepEnv(t)
				tt.setup(e)
				before := e.homeNames()
				_, err := Prepare(e.opts("s1", credLink))
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.want)
				assert.Contains(t, err.Error(), ErrClassPrepare)
				assert.Equal(t, before, e.homeNames(), "a failed decision changes nothing")
				_, err = os.Stat(filepath.Join(e.mem, ModeFileName))
				assert.True(t, os.IsNotExist(err), "no mode file on failure")
			})
		}
	})
}

// An interrupted seed is cleared, including read-only subtrees, keeping
// only the sentinel; reserved leftovers do not count as content.
func TestPrepare_InterruptedSeedCleanup(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		e := newPrepEnv(t)
		e.write(SentinelName, `{"version":1,"agent_id":"`+testAgentID+`","state":"seeding","uid":`+itoa(os.Getuid())+`,"start_id":"old"}`)
		e.write("ro/sub/file", "x")
		e.write("plain", "y")
		require.NoError(t, os.Symlink("/etc", filepath.Join(e.home, "link-out")))
		require.NoError(t, os.Chmod(filepath.Join(e.home, "ro", "sub"), 0o555))
		require.NoError(t, os.Chmod(filepath.Join(e.home, "ro"), 0o555))
		require.NoError(t, os.WriteFile(filepath.Join(e.skeleton, ".bashrc"), []byte("rc"), 0o644))

		mode, err := Prepare(e.opts("new"))
		require.NoError(t, err)
		assert.Equal(t, ModeSeed, mode)
		assert.Equal(t, []string{".bashrc", ".scion", SentinelName}, e.homeNames())
		_, err = os.Stat("/etc/passwd")
		require.NoError(t, err, "the cleanup never follows a link")
		assert.Equal(t, "new", e.sentinel().StartID)

		// Reserved leftovers on an otherwise empty home are removed.
		e2 := newPrepEnv(t)
		e2.write(".scion-home-probe.x.y", "")
		e2.write(".scion-home-seed.x.y", "")
		_, err = Prepare(e2.opts("s"))
		require.NoError(t, err)
		for _, n := range e2.homeNames() {
			assert.NotContains(t, n, ".scion-home-probe")
			assert.NotEqual(t, ".scion-home-seed.x.y", n)
		}
	})
}

func TestPrepare_SkeletonOverCapIsSkipped(t *testing.T) {
	e := newPrepEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(e.skeleton, "big"), make([]byte, 2048), 0o644))
	o := e.opts("s")
	o.SkeletonMaxBytes = 1024
	_, err := Prepare(o)
	require.NoError(t, err)
	assert.Equal(t, SkeletonSkipped, e.sentinel().Skeleton)
	_, err = os.Stat(filepath.Join(e.home, "big"))
	assert.True(t, os.IsNotExist(err), "nothing is copied over the cap")
}

func TestPrepare_WriteProbe(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	e := newPrepEnv(t)
	require.NoError(t, os.Chmod(e.home, 0o555))
	t.Cleanup(func() { _ = os.Chmod(e.home, 0o755) })
	_, err := Prepare(e.opts("s"))
	require.Error(t, err)
	var ce *ClassError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, ErrClassUnavailable, ce.Class)
	assert.Contains(t, err.Error(), "not writable by uid")
}

func TestMarkSeeded_Rechecks(t *testing.T) {
	e := newPrepEnv(t)
	_, err := Prepare(e.opts("s1"))
	require.NoError(t, err)
	err = MarkSeeded(e.home, testAgentID, "s0")
	require.Error(t, err, "a start that is not the latest one must not mark the home")
	assert.Equal(t, StateSeeding, e.sentinel().State)
	require.Error(t, MarkSeeded(e.home, "11111111-2222-4333-8444-555555555555", "s1"))
	require.NoError(t, MarkSeeded(e.home, testAgentID, "s1"))
	require.NoError(t, MarkSeeded(e.home, testAgentID, "s1"), "marking twice is fine")
	require.Error(t, MarkSeeded(filepath.Join(e.base, "missing"), testAgentID, "s1"))
	require.Error(t, MarkSeeded(e.mem, testAgentID, "s1"), "no sentinel")
}

// A cleanup that fails partway keeps the sentinel, so the home is never
// left with content and no sentinel.
func TestPrepare_InterruptedCleanupFailureKeepsSentinel(t *testing.T) {
	e := newPrepEnv(t)
	e.write(SentinelName, `{"version":1,"agent_id":"`+testAgentID+`","state":"seeding","uid":`+itoa(os.Getuid())+`,"start_id":"old"}`)
	e.write("a", "1")
	e.write("b", "2")
	calls := 0
	removeEntryTestHook = func(name string) error {
		calls++
		if calls == 2 {
			return unix.EIO
		}
		return nil
	}
	t.Cleanup(func() { removeEntryTestHook = nil })
	_, err := Prepare(e.opts("new"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), ErrClassSeed)
	s := e.sentinel()
	assert.Equal(t, StateSeeding, s.State)
	assert.Equal(t, "old", s.StartID)

	// The next start finishes the cleanup.
	removeEntryTestHook = nil
	_, err = Prepare(e.opts("newer"))
	require.NoError(t, err)
	assert.Equal(t, []string{".scion", SentinelName}, e.homeNames())
}

// mark-seeded re-reads the sentinel after writing it: a newer start that
// stamped the sentinel meanwhile makes it fail.
func TestMarkSeeded_RecheckAfterWrite(t *testing.T) {
	e := newPrepEnv(t)
	_, err := Prepare(e.opts("s1"))
	require.NoError(t, err)
	markSeededAfterWriteTestHook = func() {
		s := e.sentinel()
		s.StartID = "s2"
		data, _ := json.Marshal(s)
		require.NoError(t, os.WriteFile(filepath.Join(e.home, SentinelName), data, 0o644))
	}
	t.Cleanup(func() { markSeededAfterWriteTestHook = nil })
	err = MarkSeeded(e.home, testAgentID, "s1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "changed to start")
	require.Error(t, MarkSeeded(e.home, "not-an-id", "s2"), "the agent ID is validated")
}

// Image home directories keep their modes, read-only ones included.
func TestPrepare_SkeletonKeepsDirectoryModes(t *testing.T) {
	e := newPrepEnv(t)
	ro := filepath.Join(e.skeleton, ".ro")
	require.NoError(t, os.MkdirAll(filepath.Join(ro, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ro, "sub", "f"), []byte("x"), 0o444))
	require.NoError(t, os.Chmod(filepath.Join(ro, "sub"), 0o555))
	require.NoError(t, os.Chmod(ro, 0o550))
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755); _ = os.Chmod(filepath.Join(ro, "sub"), 0o755) })
	_, err := Prepare(e.opts("s1"))
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(e.home, ".ro"), 0o755)
		_ = os.Chmod(filepath.Join(e.home, ".ro", "sub"), 0o755)
	})
	st, err := os.Stat(filepath.Join(e.home, ".ro"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o550), st.Mode().Perm())
	st, err = os.Stat(filepath.Join(e.home, ".ro", "sub"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o555), st.Mode().Perm())
	data, err := os.ReadFile(filepath.Join(e.home, ".ro", "sub", "f"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(data))
}

// An image home that cannot be read is recorded as skipped, not copied.
func TestPrepare_UnreadableImageHome(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads any directory")
	}
	e := newPrepEnv(t)
	require.NoError(t, os.Chmod(e.skeleton, 0o000))
	t.Cleanup(func() { _ = os.Chmod(e.skeleton, 0o755) })
	_, err := Prepare(e.opts("s1"))
	require.NoError(t, err)
	assert.Equal(t, SkeletonSkipped, e.sentinel().Skeleton)
}

// A symbolic link that points inside the home is not followed by the
// beneath resolver either: seed-over does not clear hooks through a linked
// .scion, and opening a path through such a link fails.
func TestRoot_NoSymlinksEvenInsideHome(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		e := newPrepEnv(t)
		e.write(SentinelName, `{"version":1,"agent_id":"`+testAgentID+`","state":"seeded","uid":`+itoa(os.Getuid())+`,"start_id":"x"}`)
		e.write("real/hooks/keep", "k")
		require.NoError(t, os.Symlink("real", filepath.Join(e.home, ".scion")))
		_, err := Prepare(e.opts("s1"))
		require.NoError(t, err)
		_, err = os.Stat(filepath.Join(e.home, "real", "hooks", "keep"))
		require.NoError(t, err, "hooks behind a link inside the home are not cleared")

		r, err := openRoot(e.home)
		require.NoError(t, err)
		defer r.close()
		_, err = r.open(".scion/hooks/keep", unix.O_RDONLY, 0)
		require.Error(t, err)
		_, err = r.open("real/hooks/keep", unix.O_RDONLY, 0)
		require.NoError(t, err)
	})
}

// A read-only directory on a link's path (an image .config kept at 0555, or
// a user's read-only .config/gcloud) skips the link; it never wedges the
// seed or later starts, and the directory's mode is left alone.
func TestPrepare_ReadOnlyLinkParentSkips(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes into read-only directories")
	}
	bothResolvers(t, func(t *testing.T) {
		e := newPrepEnv(t)
		cfg := filepath.Join(e.skeleton, ".config")
		require.NoError(t, os.Mkdir(cfg, 0o755))
		require.NoError(t, os.Chmod(cfg, 0o555))
		t.Cleanup(func() {
			_ = os.Chmod(cfg, 0o755)
			_ = os.Chmod(filepath.Join(e.home, ".config"), 0o755)
		})
		mode, err := Prepare(e.opts("s1", credLink, secretsLink))
		require.NoError(t, err)
		assert.Equal(t, ModeSeed, mode)
		require.NoError(t, MarkSeeded(e.home, testAgentID, "s1"))
		mode, err = Prepare(e.opts("s2", credLink, secretsLink))
		require.NoError(t, err)
		assert.Equal(t, ModeSeedOver, mode)
		var res LinksResult
		e.memJSON(LinksResultFileName, &res)
		assert.Equal(t, []LinkResult{{credLink.Target, LinkSkipped}, {secretsLink.Target, LinkLinked}}, res.Links)
		st, err := os.Stat(filepath.Join(e.home, ".config"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o555), st.Mode().Perm(), "the read-only directory is not re-moded")

		// A user's read-only parent in a seeded home.
		e2 := newPrepEnv(t)
		e2.write(SentinelName, `{"version":1,"agent_id":"`+testAgentID+`","state":"seeded","uid":`+itoa(os.Getuid())+`,"start_id":"x"}`)
		gcloud := filepath.Join(e2.home, ".config", "gcloud")
		require.NoError(t, os.MkdirAll(gcloud, 0o755))
		require.NoError(t, os.Chmod(gcloud, 0o555))
		t.Cleanup(func() { _ = os.Chmod(gcloud, 0o755) })
		_, err = Prepare(e2.opts("s3", credLink))
		require.NoError(t, err)
		e2.memJSON(LinksResultFileName, &res)
		assert.Equal(t, []LinkResult{{credLink.Target, LinkSkipped}}, res.Links)
	})
}

// Skeleton directories always keep owner read and search, even when the
// image directory lacks them, so cleanup and traversal can enter them.
func TestPrepare_SkeletonDirectoriesKeepOwnerAccess(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	e := newPrepEnv(t)
	d := filepath.Join(e.skeleton, ".noexec")
	require.NoError(t, os.MkdirAll(filepath.Join(d, "child"), 0o755))
	require.NoError(t, os.Chmod(d, 0o600))
	t.Cleanup(func() {
		_ = os.Chmod(d, 0o755)
		_ = os.Chmod(filepath.Join(e.home, ".noexec"), 0o755)
	})
	_, err := Prepare(e.opts("s1"))
	require.NoError(t, err)
	st, err := os.Stat(filepath.Join(e.home, ".noexec"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), st.Mode().Perm(), "image mode 0600 plus owner read and search")

	// The interrupted-seed cleanup can enter and remove it.
	s := e.sentinel()
	require.Equal(t, StateSeeding, s.State)
	_, err = Prepare(e.opts("s2"))
	require.NoError(t, err)
}
