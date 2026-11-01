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

package projectfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The project prefix is exactly [hubs/<hub>/]workspaces/<uuid>/; the agent
// prefix is that plus <agentID>/. Unsafe elements never widen the prefix.
func TestObjectPrefixes(t *testing.T) {
	const pid = "550e8400-e29b-41d4-a716-446655440000"
	const aid = "770e8400-e29b-41d4-a716-446655440000"
	p, err := ProjectObjectPrefix("hub-1", pid)
	require.NoError(t, err)
	assert.Equal(t, "hubs/hub-1/workspaces/"+pid+"/", p)
	p, err = ProjectObjectPrefix("", pid)
	require.NoError(t, err)
	assert.Equal(t, "workspaces/"+pid+"/", p)
	a, err := AgentObjectPrefix("hub-1", pid, aid)
	require.NoError(t, err)
	assert.Equal(t, "hubs/hub-1/workspaces/"+pid+"/"+aid+"/", a)

	for _, bad := range [][2]string{{"hub-1", ""}, {"hub-1", ".."}, {"hub-1", "a/b"}, {"a/b", pid}, {"..", pid}} {
		_, err := ProjectObjectPrefix(bad[0], bad[1])
		assert.Error(t, err, "%q", bad)
	}
	for _, bad := range []string{"", "..", "x/y", "dev"} {
		_, err := AgentObjectPrefix("hub-1", pid, bad)
		assert.Error(t, err, "agent %q", bad)
	}
}

type fakeObjects struct {
	prefixes []string
	err      error
}

func (f *fakeObjects) DeletePrefix(_ context.Context, prefix string) error {
	f.prefixes = append(f.prefixes, prefix)
	return f.err
}

// DeleteObjectPrefix deletes exactly the prefix it is given and refuses
// anything that is not a workspaces/ directory prefix.
func TestDeleteObjectPrefix(t *testing.T) {
	f := &fakeObjects{}
	d := DeleteObjectPrefix(context.Background(), f, "hubs/h/workspaces/p/")
	assert.Equal(t, OutcomeDeleted, d.Outcome)
	assert.Equal(t, []string{"hubs/h/workspaces/p/"}, f.prefixes)

	for _, bad := range []string{"", "hubs/h/", "hubs/h/workspaces/p", "templates/x/"} {
		d := DeleteObjectPrefix(context.Background(), f, bad)
		assert.Equal(t, OutcomeFailed, d.Outcome, "prefix %q", bad)
	}
	assert.Len(t, f.prefixes, 1, "a refused prefix never reaches the store")

	f.err = errors.New("boom")
	d = DeleteObjectPrefix(context.Background(), f, "hubs/h/workspaces/p/")
	assert.Equal(t, OutcomeFailed, d.Outcome)
	assert.ErrorContains(t, d.Err, "boom")
}

// One audit line per deletion names the deleter, the tree, the project,
// the path and the outcome; failures log at ERROR, kept trees at WARN.
func TestAuditLines(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	AuditAll(context.Background(), logger, DeleterHub, "pid", "dev", []Deletion{
		{Tree: TreeAgentRoot, Path: "/mnt/s/projects/pid/agents/dev", Outcome: OutcomeDeleted},
		{Tree: TreeLegacyHubProjects, Path: "/mnt/s/hub-projects/demo", Outcome: OutcomeKept, Reason: "no identity marker"},
		{Tree: TreeObjectPrefix, Path: "hubs/h/workspaces/pid/", Outcome: OutcomeFailed, Err: errors.New("denied")},
	})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 3)
	for _, l := range lines {
		assert.Contains(t, l, `msg="`+AuditMessage+`"`)
		assert.Contains(t, l, "audit=storage_delete")
		assert.Contains(t, l, "deleter=hub")
		assert.Contains(t, l, "project_id=pid")
		assert.Contains(t, l, "agent=dev")
	}
	assert.Contains(t, lines[0], "level=INFO")
	assert.Contains(t, lines[0], "tree=agent-root")
	assert.Contains(t, lines[0], "outcome=deleted")
	assert.Contains(t, lines[1], "level=WARN")
	assert.Contains(t, lines[1], `reason="no identity marker"`)
	assert.Contains(t, lines[2], "level=ERROR")
	assert.Contains(t, lines[2], "error=denied")
}

// Redact makes a deletion fit for a log without host paths: paths become
// relative to a storage root, bases leave the reason, errors become their
// class; object prefixes are kept.
func TestRedact(t *testing.T) {
	d := Redact(Deletion{
		Tree: TreeProjectRoot, Path: "/home/u/mnt/share1/projects/pid", Outcome: OutcomeFailed,
		Reason: "checked /home/u/.scion/hub-workspaces/demo",
		Err:    fmt.Errorf("%w: %w: lstat /home/u/mnt: no such file", ErrRefused, ErrNoProjectTree),
	}, "/home/u/mnt/share1", "/home/u/.scion")
	assert.Equal(t, "projects/pid", d.Path)
	assert.Equal(t, "checked hub-workspaces/demo", d.Reason)
	assert.NotContains(t, d.Err.Error(), "/home/u")
	assert.Contains(t, d.Err.Error(), "share not mounted")

	assert.Equal(t, "pid", Redact(Deletion{Path: "/elsewhere/pid"}, "/home/u").Path, "outside every base: last element only")
	assert.Equal(t, "hubs/h/workspaces/p/", Redact(Deletion{Path: "hubs/h/workspaces/p/"}, "/home/u").Path)
	assert.Equal(t, "removal failed", Redact(Deletion{Err: errors.New("x /home/u/y")}).Err.Error())
	assert.Equal(t, "timed out", Redact(Deletion{Err: fmt.Errorf("x: %w", context.DeadlineExceeded)}).Err.Error())
	assert.Equal(t, "removal failed: "+syscall.ENOTEMPTY.Error(),
		Redact(Deletion{Err: &os.PathError{Op: "unlinkat", Path: "/home/u/x", Err: syscall.ENOTEMPTY}}).Err.Error())
	assert.Equal(t, "object storage delete failed", Redact(Deletion{Tree: TreeObjectPrefix, Err: errors.New("gs://b/x denied")}).Err.Error())
}
