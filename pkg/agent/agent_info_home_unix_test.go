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

package agent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// agent-info.json reads and writes are confined to the agent home: a link
// (or a second hard link) at agent-info.json is refused, the file it points
// to is never read into the home, and that file is left unchanged.

// outsideInfoMarker is a field value only the outside file holds.
const outsideInfoMarker = "outside-only-marker-7f3a"

type agentInfoFixture struct {
	projectDir, agentName, home, infoPath, outside, outsideInfo, before string
}

func newAgentInfoFixture(t *testing.T) *agentInfoFixture {
	t.Helper()
	base := t.TempDir()
	f := &agentInfoFixture{
		projectDir: filepath.Join(base, "proj", config.DotScion),
		agentName:  "info-agent",
		outside:    filepath.Join(base, "outside"),
	}
	f.home = config.GetAgentHomePath(f.projectDir, f.agentName)
	f.infoPath = filepath.Join(f.home, agentInfoFileName)
	f.outsideInfo = filepath.Join(f.outside, "info.json")
	for _, d := range []string{f.home, f.outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	content := `{"name":"other","phase":"running","template":"` + outsideInfoMarker + `"}`
	if err := os.WriteFile(f.outsideInfo, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f.before = outsideSnapshot(t, f.outside)
	return f
}

// checkConfined asserts the outside tree is unchanged, agent-info.json in
// the home is still not a regular file of its own (the planted entry was
// left alone, nothing installed from the outside file), and no file in the
// home carries the outside file's content.
func (f *agentInfoFixture) checkConfined(t *testing.T) {
	t.Helper()
	if got := outsideSnapshot(t, f.outside); got != f.before {
		t.Fatalf("outside directory changed:\n%s\nwant:\n%s", got, f.before)
	}
	err := filepath.WalkDir(f.home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if p == f.infoPath {
			// A hard link to the outside file is the outside file; its
			// content is covered by the outside snapshot.
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), outsideInfoMarker) {
			t.Errorf("%s holds content read from the outside file", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *agentInfoFixture) plantLink(t *testing.T) {
	t.Helper()
	_ = os.Remove(f.infoPath)
	if err := os.Symlink(f.outsideInfo, f.infoPath); err != nil {
		t.Fatal(err)
	}
}

func (f *agentInfoFixture) requireLink(t *testing.T) {
	t.Helper()
	fi, err := os.Lstat(f.infoPath)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("agent-info.json = %v, %v; want the planted link left as is", fi, err)
	}
}

func TestUpdateAgentConfig_AgentInfoLinkRefusedNotRead(t *testing.T) {
	f := newAgentInfoFixture(t)
	f.plantLink(t)
	err := UpdateAgentConfig(f.agentName, f.projectDir, "running", "docker", "p")
	if !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("err = %v, want the link at agent-info.json refused (ELOOP)", err)
	}
	f.requireLink(t)
	f.checkConfined(t)
}

func TestUpdateAgentConfig_AgentInfoHardLinkRefused(t *testing.T) {
	f := newAgentInfoFixture(t)
	if err := os.Link(f.outsideInfo, f.infoPath); err != nil {
		t.Skipf("hard link not possible here: %v", err)
	}
	err := UpdateAgentConfig(f.agentName, f.projectDir, "running", "docker", "p")
	if !errors.Is(err, dirfd.ErrNotSingleLinkRegular) {
		t.Fatalf("err = %v, want a hard-linked agent-info.json refused", err)
	}
	f.checkConfined(t)
}

func TestUpdateAgentConfig_AgentInfoLinkReplantedBetweenUpdatesIsRefused(t *testing.T) {
	f := newAgentInfoFixture(t)
	if err := os.WriteFile(f.infoPath, []byte(`{"name":"info-agent","phase":"created"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := UpdateAgentConfig(f.agentName, f.projectDir, "running", "docker", "p"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(f.infoPath)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o640 {
		t.Fatalf("agent-info.json = %v, %v; want a regular file keeping mode 0640", fi, err)
	}
	if b, _ := os.ReadFile(f.infoPath); !strings.Contains(string(b), `"phase": "running"`) && !strings.Contains(string(b), `"phase":"running"`) {
		t.Fatalf("agent-info.json not updated: %s", b)
	}
	f.plantLink(t)
	if err := UpdateAgentConfig(f.agentName, f.projectDir, "error", "docker", "p"); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("err = %v, want the re-planted link refused (ELOOP)", err)
	}
	f.requireLink(t)
	f.checkConfined(t)
}

func TestPersistAgentInfoState_AgentInfoLinkRefusedNotRead(t *testing.T) {
	f := newAgentInfoFixture(t)
	f.plantLink(t)
	if err := persistAgentInfoState(f.infoPath, "stopped", ""); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("err = %v, want the link at agent-info.json refused (ELOOP)", err)
	}
	f.requireLink(t)
	f.checkConfined(t)
}

func TestReadHomeAgentInfo_LinkRefused(t *testing.T) {
	f := newAgentInfoFixture(t)
	f.plantLink(t)
	data, err := readHomeAgentInfo(f.home)
	if !errors.Is(err, syscall.ELOOP) || data != nil {
		t.Fatalf("readHomeAgentInfo = %q, %v; want the link refused", data, err)
	}
	if _, err := readHomeAgentInfo(filepath.Join(f.home, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing home: err = %v, want ErrNotExist", err)
	}
	f.checkConfined(t)
}

// writeOutsideInfo replaces the outside file's content and refreshes the
// fixture's snapshot.
func (f *agentInfoFixture) writeOutsideInfo(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(f.outsideInfo, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f.before = outsideSnapshot(t, f.outside)
}

func (f *agentInfoFixture) plantFIFO(t *testing.T) {
	t.Helper()
	_ = os.Remove(f.infoPath)
	if err := syscall.Mkfifo(f.infoPath, 0o644); err != nil {
		t.Fatal(err)
	}
}

// within fails the test if fn does not return within 10 s (a FIFO read
// that blocks).
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s blocked on a FIFO at agent-info.json", what)
	}
}

func TestReadSavedAgentInfo_LinkAndFIFOUnreadable(t *testing.T) {
	f := newAgentInfoFixture(t)
	f.writeOutsideInfo(t, `{"name":"x","profile":"`+outsideInfoMarker+`"}`)
	f.plantLink(t)
	info, st := readSavedAgentInfo(f.agentName, f.projectDir)
	if info != nil || st != SavedProfileUnreadable {
		t.Fatalf("link: readSavedAgentInfo = %+v, %v; want nil, SavedProfileUnreadable", info, st)
	}
	if GetSavedProfile(f.agentName, f.projectDir) == outsideInfoMarker {
		t.Fatal("GetSavedProfile returned the profile of the file outside the home")
	}
	f.requireLink(t)
	f.checkConfined(t)

	f.plantFIFO(t)
	within(t, "readSavedAgentInfo", func() {
		if info, st := readSavedAgentInfo(f.agentName, f.projectDir); info != nil || st != SavedProfileUnreadable {
			t.Errorf("FIFO: readSavedAgentInfo = %+v, %v; want nil, SavedProfileUnreadable", info, st)
		}
	})
	f.checkConfined(t)
}

func TestReadStoredAgentRecord_LinkAndFIFORefused(t *testing.T) {
	f := newAgentInfoFixture(t)
	f.plantLink(t)
	if rec := readStoredAgentRecord(f.home); rec != nil {
		t.Fatalf("link: readStoredAgentRecord = %+v; want nil (outside record not read)", rec)
	}
	f.checkConfined(t)
	f.plantFIFO(t)
	within(t, "readStoredAgentRecord", func() {
		if rec := readStoredAgentRecord(f.home); rec != nil {
			t.Errorf("FIFO: readStoredAgentRecord = %+v; want nil", rec)
		}
	})
}

func TestRetainedAgentInfoMarked_LinkAndFIFORefused(t *testing.T) {
	f := newAgentInfoFixture(t)
	f.writeOutsideInfo(t, `{"name":"x","phase":"`+softDeletedPhase+`"}`)
	f.plantLink(t)
	if retainedAgentInfoMarked(f.infoPath) {
		t.Fatal("link: the soft-delete mark of the file outside the home was used")
	}
	f.checkConfined(t)
	f.plantFIFO(t)
	within(t, "retainedAgentInfoMarked", func() {
		if retainedAgentInfoMarked(f.infoPath) {
			t.Error("FIFO: marked = true")
		}
	})
}

func TestLegacyAgentInfoRunID_LinkAndFIFORefused(t *testing.T) {
	f := newAgentInfoFixture(t)
	f.writeOutsideInfo(t, `{"name":"x","runId":"outside-run-1"}`)
	f.plantLink(t)
	if got := legacyAgentInfoRunID(f.projectDir, f.agentName); got != "" {
		t.Fatalf("link: legacyAgentInfoRunID = %q; want \"\" (outside file not read)", got)
	}
	f.checkConfined(t)
	f.plantFIFO(t)
	within(t, "legacyAgentInfoRunID", func() {
		if got := legacyAgentInfoRunID(f.projectDir, f.agentName); got != "" {
			t.Errorf("FIFO: legacyAgentInfoRunID = %q", got)
		}
	})
	// Control: the same content as a regular file in the home is read.
	_ = os.Remove(f.infoPath)
	if err := os.WriteFile(f.infoPath, []byte(`{"name":"x","runId":"outside-run-1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := legacyAgentInfoRunID(f.projectDir, f.agentName); got != "outside-run-1" {
		t.Fatalf("regular file: legacyAgentInfoRunID = %q; want outside-run-1", got)
	}
}

func TestUpdateAgentConfig_AgentInfoFIFODoesNotBlock(t *testing.T) {
	f := newAgentInfoFixture(t)
	f.plantFIFO(t)
	within(t, "UpdateAgentConfig", func() {
		if err := UpdateAgentConfig(f.agentName, f.projectDir, "running", "", ""); !errors.Is(err, dirfd.ErrNotSingleLinkRegular) {
			t.Errorf("err = %v, want a FIFO refused", err)
		}
	})
}
