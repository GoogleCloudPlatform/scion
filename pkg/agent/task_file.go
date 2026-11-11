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

package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TaskFileRel is where, relative to the agent home, the full text of the
// agent's initial task is written on every start that has a task. Inside
// the agent it is ~/.scion/task.md.
const TaskFileRel = ".scion/task.md"

// TaskFileContainerPath is TaskFileRel as the agent sees it.
const TaskFileContainerPath = "~/" + TaskFileRel

// InlineTaskMaxBytes is the largest task passed to the harness inline,
// measured as the bytes it takes in the tmux command that starts the
// harness once quoted for the shell (runtime.QuotedTaskBytes), where each
// single quote takes 13 bytes. tmux rejects commands over about 16 KB, so
// a larger task is replaced by a short task that points to
// TaskFileContainerPath.
const InlineTaskMaxBytes = 8 * 1024

// deliverTaskFile writes task to TaskFileRel under agentHome, replacing
// any file left by an earlier start, and returns the task to pass to the
// harness: task itself when its quoted size is at most InlineTaskMaxBytes,
// otherwise a short task that points to the file. An empty task writes
// nothing and is returned unchanged.
func deliverTaskFile(agentHome, task string) (string, error) {
	if task == "" || agentHome == "" {
		return task, nil
	}
	if err := writeTaskFile(agentHome, task); err != nil {
		return "", err
	}
	if runtime.QuotedTaskBytes(task) <= InlineTaskMaxBytes {
		return task, nil
	}
	return taskFilePointer(len(task)), nil
}

// writeTaskFile writes task to TaskFileRel under agentHome without
// following symbolic links. .scion must be a real directory in agentHome
// (it is created with mode 0700 when missing), and the file is written
// to a new temporary file in it, set to mode 0644 and renamed over
// task.md, so a link at either path is refused or replaced, never
// written through. Nothing is written outside agentHome.
func writeTaskFile(agentHome, task string) error {
	home, err := os.OpenRoot(agentHome)
	if err != nil {
		return fmt.Errorf("failed to open the agent home for the task file: %w", err)
	}
	defer func() { _ = home.Close() }()

	const dirName = ".scion"
	if err := home.Mkdir(dirName, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("failed to create the task file directory: %w", err)
	}
	info, err := home.Lstat(dirName)
	if err != nil {
		return fmt.Errorf("failed to check the task file directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("the task file directory %s in the agent home is not a directory (mode %v); not writing the task file", dirName, info.Mode().Type())
	}
	dir, err := home.OpenRoot(dirName)
	if err != nil {
		return fmt.Errorf("failed to open the task file directory: %w", err)
	}
	defer func() { _ = dir.Close() }()
	// The directory opened must be the one checked above, not a link
	// put in its place since.
	if opened, err := dir.Stat("."); err != nil || !os.SameFile(info, opened) {
		return fmt.Errorf("the task file directory %s in the agent home changed while it was opened; not writing the task file", dirName)
	}

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("failed to name the temporary task file: %w", err)
	}
	const fileName = "task.md"
	tmpName := "." + fileName + "." + hex.EncodeToString(suffix[:])
	f, err := dir.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("failed to create the temporary task file: %w", err)
	}
	installed := false
	defer func() {
		if !installed {
			_ = dir.Remove(tmpName)
		}
	}()
	_, werr := f.WriteString(task)
	if werr == nil {
		// Set the mode on the open file: the create mode is subject to
		// the umask.
		werr = f.Chmod(0o644)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("failed to write the task file: %w", werr)
	}
	if err := dir.Rename(tmpName, fileName); err != nil {
		return fmt.Errorf("failed to install the task file: %w", err)
	}
	installed = true
	return nil
}

// taskFilePointer is the task given to the harness in place of a task of
// size bytes that is too large to pass inline.
func taskFilePointer(size int) string {
	return fmt.Sprintf("Your task is in the file %s (%d bytes). "+
		"Read the whole file before you start, then carry out the task it describes.",
		TaskFileContainerPath, size)
}
