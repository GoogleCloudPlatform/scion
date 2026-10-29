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

package maintenance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/version"
	"github.com/GoogleCloudPlatform/scion/pkg/version/update"
)

// fakeDBSnapshotter is a DBSnapshotter that records its calls (into calls,
// when set, so they interleave with a fakeInstaller's).
type fakeDBSnapshotter struct {
	unsupported string // reason; "" = supported
	err         error
	calls       *[]string
	froms       []string
}

const fakeSnapshotDir = "/srv/scion/snapshots"

func (f *fakeDBSnapshotter) SnapshotStatus() DBSnapshotStatus {
	if f.unsupported != "" {
		return DBSnapshotStatus{Reason: f.unsupported}
	}
	return DBSnapshotStatus{Supported: true, Dir: fakeSnapshotDir}
}

func (f *fakeDBSnapshotter) Snapshot(_ context.Context, from string, _ io.Writer) (string, error) {
	if f.calls != nil {
		*f.calls = append(*f.calls, "snapshot")
	}
	f.froms = append(f.froms, from)
	if f.err != nil {
		return "", f.err
	}
	return fakeSnapshotDir + "/hub-" + snapshotVersionLabel(from) + "-20261029T070000Z.db", nil
}

// runHooked runs e with a before-restart hook and returns what it got.
func runHooked(e *BinaryUpdateExecutor, params map[string]string) (*PendingRestart, string, error) {
	var hooked *PendingRestart
	var log bytes.Buffer
	err := e.RunWithBeforeRestart(context.Background(), &log, params,
		func(_ context.Context, p PendingRestart) error {
			hooked = &p
			return nil
		})
	return hooked, log.String(), err
}

func TestBinaryUpdate_DowngradeConfirmation(t *testing.T) {
	requireLinux(t)

	setup := func(t *testing.T, current string) (*fakeGitHub, *fakeInstaller, *fakeDBSnapshotter, *BinaryUpdateExecutor) {
		gh := newFakeGitHub(t)
		gh.addRelease("nightly-20261020", "2026-10-20T04:00:00Z", "nightly-20261020")
		gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
		gh.addRelease("v0.3.0-preview.4", "2026-10-22T18:00:00Z", "v0.3.0-preview.4")
		inst := &fakeInstaller{}
		snaps := &fakeDBSnapshotter{calls: &inst.calls}
		e := newTestExecutor(gh, inst, current)
		e.snapshots = snaps
		return gh, inst, snaps, e
	}

	t.Run("missing confirmation: 409 with the risk list, nothing downloaded", func(t *testing.T) {
		gh, inst, snaps, e := setup(t, "nightly-20261023")
		hooked, _, err := runHooked(e, map[string]string{"target_version": "nightly-20261020"})
		wantCode(t, err, CodeDowngradeRequiresConfirmation)
		re, _ := IsRequestError(err)
		if re.HTTPStatus() != 409 {
			t.Fatalf("status = %d", re.HTTPStatus())
		}
		risks, _ := re.Details["risks"].([]string)
		if len(risks) < 5 || re.Details["snapshotPath"] != fakeSnapshotDir || re.Details["class"] != "downgrade" ||
			re.Details["target"] != "nightly-20261020" || re.Details["floor"] != version.DowngradeFloor {
			t.Fatalf("details = %#v", re.Details)
		}
		if hooked != nil || len(inst.calls) != 0 || len(snaps.froms) != 0 || gh.requestCount() != 0 {
			t.Fatalf("calls = %v, snapshots = %v, requests = %d; want a refusal before preflight", inst.calls, snaps.froms, gh.requestCount())
		}
	})

	t.Run("mismatched confirmation", func(t *testing.T) {
		_, inst, _, e := setup(t, "nightly-20261023")
		_, _, err := runHooked(e, map[string]string{"target_version": "nightly-20261020", "confirm_downgrade": "nightly-20261021"})
		wantCode(t, err, CodeDowngradeRequiresConfirmation)
		if len(inst.calls) != 0 {
			t.Fatalf("calls = %v", inst.calls)
		}
	})

	t.Run("correct confirmation installs after a snapshot", func(t *testing.T) {
		_, inst, snaps, e := setup(t, "nightly-20261023")
		hooked, log, err := runHooked(e, map[string]string{"target_version": "nightly-20261020", "confirm_downgrade": "nightly-20261020"})
		if err != nil {
			t.Fatalf("Run: %v\n%s", err, log)
		}
		want := []string{"preflight", "snapshot", "stage:nightly-20261020", "activate:staged-nightly-20261020"}
		if !slices.Equal(inst.calls[:4], want) {
			t.Fatalf("calls = %v, want prefix %v", inst.calls, want)
		}
		if hooked == nil || hooked.To != "nightly-20261020" ||
			hooked.Snapshot != fakeSnapshotDir+"/hub-nightly-20261023-20261029T070000Z.db" {
			t.Fatalf("hook = %+v", hooked)
		}
		if !slices.Equal(snaps.froms, []string{"nightly-20261023"}) {
			t.Fatalf("snapshot from = %v", snaps.froms)
		}
	})

	t.Run("switch-older: refused without, installed with confirmation", func(t *testing.T) {
		gh, inst, _, e := setup(t, "nightly-20261023")
		_, _, err := runHooked(e, map[string]string{"target_version": "v0.3.0-preview.4"})
		wantCode(t, err, CodeDowngradeRequiresConfirmation)
		if re, _ := IsRequestError(err); re.Details["class"] != "switch-older" {
			t.Fatalf("class = %v", re.Details["class"])
		}
		if gh.requested("/" + BinaryAssetName()) {
			t.Fatal("downloaded a refused target")
		}
		inst.calls = nil
		hooked, _, err := runHooked(e, map[string]string{"target_version": "v0.3.0-preview.4", "confirm_downgrade": "v0.3.0-preview.4"})
		if err != nil || hooked == nil || hooked.To != "v0.3.0-preview.4" {
			t.Fatalf("err = %v, hook = %+v", err, hooked)
		}
	})

	t.Run("latest:<channel> needs the resolved tag", func(t *testing.T) {
		_, _, _, e := setup(t, "nightly-20261023")
		_, _, err := runHooked(e, map[string]string{"target_version": "latest:preview", "confirm_downgrade": "latest:preview"})
		wantCode(t, err, CodeInvalidParam) // not a release tag
		_, _, err = runHooked(e, map[string]string{"target_version": "latest:preview"})
		wantCode(t, err, CodeDowngradeRequiresConfirmation)
		hooked, _, err := runHooked(e, map[string]string{"target_version": "latest:preview", "confirm_downgrade": "v0.3.0-preview.4"})
		if err != nil || hooked == nil || hooked.To != "v0.3.0-preview.4" {
			t.Fatalf("err = %v, hook = %+v", err, hooked)
		}
	})
}

func TestBinaryUpdate_DowngradeFloor(t *testing.T) {
	requireLinux(t)

	t.Run("nightly below the floor is refused without any request", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("nightly-20261008", "2026-10-08T04:00:00Z", "nightly-20261008")
		inst := &fakeInstaller{}
		e := newTestExecutor(gh, inst, "nightly-20261023")
		e.snapshots = &fakeDBSnapshotter{}
		err := e.Run(context.Background(), &bytes.Buffer{},
			map[string]string{"target_version": "nightly-20261008", "confirm_downgrade": "nightly-20261008"})
		wantCode(t, err, CodeBelowDowngradeFloor)
		if gh.requestCount() != 0 || len(inst.calls) != 0 {
			t.Fatalf("requests = %d, calls = %v", gh.requestCount(), inst.calls)
		}
	})

	t.Run("preview published before the floor is refused before download", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("v0.3.0-preview.1", "2026-10-01T18:00:00Z", "v0.3.0-preview.1")
		inst := &fakeInstaller{}
		e := newTestExecutor(gh, inst, "nightly-20261023")
		e.snapshots = &fakeDBSnapshotter{}
		err := e.Run(context.Background(), &bytes.Buffer{},
			map[string]string{"target_version": "v0.3.0-preview.1", "confirm_downgrade": "v0.3.0-preview.1"})
		wantCode(t, err, CodeBelowDowngradeFloor)
		if gh.requested("/" + BinaryAssetName()) {
			t.Fatal("downloaded a target below the floor")
		}
	})

	t.Run("the floor itself is installable", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("nightly-20261015", "2026-10-15T04:00:00Z", "nightly-20261015")
		e := newTestExecutor(gh, &fakeInstaller{}, "nightly-20261023")
		e.snapshots = &fakeDBSnapshotter{}
		e.floor = "nightly-20261015"
		if _, _, err := runHooked(e, map[string]string{"target_version": "nightly-20261015", "confirm_downgrade": "nightly-20261015"}); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})

	t.Run("the option overrides the default floor", func(t *testing.T) {
		e := NewBinaryUpdateExecutor(BinaryUpdateOptions{CurrentVersion: "nightly-20261023", DowngradeFloor: "nightly-20261020",
			Snapshotter: &fakeDBSnapshotter{}})
		wantCode(t, e.ValidateParams(map[string]string{"target_version": "nightly-20261019", "confirm_downgrade": "nightly-20261019"}),
			CodeBelowDowngradeFloor)
		if err := e.ValidateParams(map[string]string{"target_version": "nightly-20261021", "confirm_downgrade": "nightly-20261021"}); err != nil {
			t.Fatal(err)
		}
		if NewBinaryUpdateExecutor(BinaryUpdateOptions{}).floor != version.DowngradeFloor {
			t.Fatal("default floor is not version.DowngradeFloor")
		}
	})
}

func TestBinaryUpdate_SnapshotBeforeEveryUpdate(t *testing.T) {
	requireLinux(t)
	newExec := func(t *testing.T, snaps *fakeDBSnapshotter) (*fakeInstaller, *BinaryUpdateExecutor) {
		gh := newFakeGitHub(t)
		gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
		inst := &fakeInstaller{}
		snaps.calls = &inst.calls
		e := newTestExecutor(gh, inst, "nightly-20261022")
		e.snapshots = snaps
		return inst, e
	}
	upgrade := map[string]string{"target_version": "nightly-20261023"}

	t.Run("an upgrade is snapshotted too", func(t *testing.T) {
		inst, e := newExec(t, &fakeDBSnapshotter{})
		hooked, _, err := runHooked(e, upgrade)
		if err != nil {
			t.Fatal(err)
		}
		if inst.calls[1] != "snapshot" || hooked.Snapshot == "" {
			t.Fatalf("calls = %v, hook = %+v", inst.calls, hooked)
		}
	})

	t.Run("a failed snapshot installs nothing", func(t *testing.T) {
		inst, e := newExec(t, &fakeDBSnapshotter{err: errors.New("not enough free disk space")})
		hooked, _, err := runHooked(e, upgrade)
		if err == nil || !strings.Contains(err.Error(), "nothing was installed") || !strings.Contains(err.Error(), "free disk space") {
			t.Fatalf("err = %v", err)
		}
		if hooked != nil || !slices.Equal(inst.calls, []string{"preflight", "snapshot"}) {
			t.Fatalf("calls = %v", inst.calls)
		}
	})

	t.Run("unsupported (PostgreSQL): the upgrade goes ahead without one", func(t *testing.T) {
		inst, e := newExec(t, &fakeDBSnapshotter{unsupported: "PostgreSQL database"})
		hooked, log, err := runHooked(e, upgrade)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(inst.calls, "snapshot") || hooked.Snapshot != "" {
			t.Fatalf("calls = %v, hook = %+v", inst.calls, hooked)
		}
		if !strings.Contains(log, "No database snapshot taken: PostgreSQL database") {
			t.Fatalf("log lacks the skipped snapshot:\n%s", log)
		}
	})
}

func TestBinaryUpdate_DowngradeUnsupportedDriver(t *testing.T) {
	e := NewBinaryUpdateExecutor(BinaryUpdateOptions{CurrentVersion: "nightly-20261023",
		Snapshotter: &fakeDBSnapshotter{unsupported: "PostgreSQL database"}})
	for _, params := range []map[string]string{
		{"target_version": "nightly-20261020"},
		{"target_version": "nightly-20261020", "confirm_downgrade": "nightly-20261020"},
	} {
		err := e.ValidateParams(params)
		wantCode(t, err, CodeDowngradeUnsupportedDriver)
		if !strings.Contains(err.Error(), "PostgreSQL") {
			t.Fatalf("message = %q", err)
		}
	}
	// An upgrade is not affected.
	if err := e.ValidateParams(map[string]string{"target_version": "nightly-20261024"}); err != nil {
		t.Fatal(err)
	}
	// No snapshotter at all counts as unsupported.
	wantCode(t, NewBinaryUpdateExecutor(BinaryUpdateOptions{CurrentVersion: "nightly-20261023"}).
		ValidateParams(map[string]string{"target_version": "nightly-20261020", "confirm_downgrade": "nightly-20261020"}),
		CodeDowngradeUnsupportedDriver)

	t.Run("switch-older at run time", func(t *testing.T) {
		requireLinux(t)
		gh := newFakeGitHub(t)
		gh.addRelease("v0.3.0-preview.4", "2026-10-22T18:00:00Z", "v0.3.0-preview.4")
		e := newTestExecutor(gh, &fakeInstaller{}, "nightly-20261023")
		e.snapshots = &fakeDBSnapshotter{unsupported: "PostgreSQL database"}
		err := e.Run(context.Background(), &bytes.Buffer{},
			map[string]string{"target_version": "v0.3.0-preview.4", "confirm_downgrade": "v0.3.0-preview.4"})
		wantCode(t, err, CodeDowngradeUnsupportedDriver)
	})
}

func TestBinaryUpdate_CheckTarget(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
	gh.addRelease("nightly-20261024", "2026-10-24T04:00:00Z", "nightly-20261024")
	gh.addRelease("v0.3.0-preview.4", "2026-10-22T18:00:00Z", "v0.3.0-preview.4")
	gh.addRelease("v0.3.0-preview.1", "2026-10-01T18:00:00Z", "v0.3.0-preview.1")
	inst := &fakeInstaller{}
	e := newTestExecutor(gh, inst, "nightly-20261023")
	e.snapshots = &fakeDBSnapshotter{}
	ctx := context.Background()

	tests := []struct {
		name   string
		params map[string]string
		code   string
	}{
		{"upgrade", map[string]string{"target_version": "nightly-20261024"}, ""},
		{"cross-channel older, unconfirmed", map[string]string{"target_version": "v0.3.0-preview.4"}, CodeDowngradeRequiresConfirmation},
		{"cross-channel older, confirmed", map[string]string{"target_version": "v0.3.0-preview.4", "confirm_downgrade": "v0.3.0-preview.4"}, ""},
		{"latest resolving older, unconfirmed", map[string]string{"target_version": "latest:preview"}, CodeDowngradeRequiresConfirmation},
		{"latest resolving older, confirmed", map[string]string{"target_version": "latest:preview", "confirm_downgrade": "v0.3.0-preview.4"}, ""},
		{"cross-channel below the floor", map[string]string{"target_version": "v0.3.0-preview.1", "confirm_downgrade": "v0.3.0-preview.1"}, CodeBelowDowngradeFloor},
		{"same", map[string]string{"target_version": "nightly-20261023"}, CodeAlreadyRunning},
		{"unknown param", map[string]string{"target_version": "nightly-20261024", "download_url": "x"}, CodeUnknownParam},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := e.CheckTarget(ctx, tt.params)
			if tt.code == "" {
				if err != nil {
					t.Fatalf("CheckTarget: %v", err)
				}
				return
			}
			wantCode(t, err, tt.code)
		})
	}
	if len(inst.calls) != 0 || gh.requested("/"+BinaryAssetName()) {
		t.Fatalf("CheckTarget ran the installer (%v) or downloaded", inst.calls)
	}

	t.Run("an unreachable GitHub is not a refusal", func(t *testing.T) {
		err := e.CheckTarget(ctx, map[string]string{"target_version": "nightly-20261030"})
		if err == nil {
			t.Fatal("want a lookup error")
		}
		if _, ok := IsRequestError(err); ok {
			t.Fatalf("err = %v is a request error; a lookup failure must not refuse the request", err)
		}
	})
}

func TestDowngradeDetails(t *testing.T) {
	st := DBSnapshotStatus{Supported: true, Dir: "/opt/scion/snapshots"}
	d := DowngradeDetails("nightly-20261023", "v0.3.0-preview.4", update.ClassSwitchOlder, "nightly-20261009", st)
	risks, ok := d["risks"].([]string)
	if !ok {
		t.Fatalf("risks = %#v", d["risks"])
	}
	all := strings.Join(risks, "\n")
	for _, want := range []string{
		"v0.3.0-preview.4 (preview channel) was published before the running version nightly-20261023",
		"not migrated down",
		"Harness bundles",
		"settings.yaml",
		"plugin",
		"/opt/scion/snapshots",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("risks lack %q:\n%s", want, all)
		}
	}
	if d["snapshotPath"] != "/opt/scion/snapshots" || d["current"] != "nightly-20261023" || d["class"] != "switch-older" {
		t.Fatalf("details = %#v", d)
	}
	if g := DowngradeDetails("", "nightly-20261020", update.ClassSwitchOlder, "nightly-20261009", st); g["current"] != "(git build)" {
		t.Fatalf("git build current = %v", g["current"])
	}
}

func TestUpdateBinaryParamsAllowConfirmDowngrade(t *testing.T) {
	if !slices.Contains(UpdateBinaryParams, ParamConfirmDowngrade) || ParamConfirmDowngrade != "confirm_downgrade" {
		t.Fatalf("UpdateBinaryParams = %v", UpdateBinaryParams)
	}
	if err := CheckAllowedParams(map[string]string{"target_version": "", "confirm_downgrade": ""}, UpdateBinaryParams); err != nil {
		t.Fatal(err)
	}
	wantCode(t, CheckAllowedParams(map[string]string{"target_version": "", "force": ""}, UpdateBinaryParams), CodeUnknownParam)
}
