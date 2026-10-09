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
	"fmt"
	"os"
	"path/filepath"
)

// TaskFileRel is where, relative to the agent home, the full text of the
// agent's initial task is written on every start that has a task. Inside
// the agent it is ~/.scion/task.md.
const TaskFileRel = ".scion/task.md"

// TaskFileContainerPath is TaskFileRel as the agent sees it.
const TaskFileContainerPath = "~/" + TaskFileRel

// InlineTaskMaxBytes is the largest task passed to the harness inline.
// The task is part of the tmux command that starts the harness, and tmux
// rejects commands over about 16 KB, so a larger task is replaced by a
// short task that points to TaskFileContainerPath.
const InlineTaskMaxBytes = 8 * 1024

// deliverTaskFile writes task to TaskFileRel under agentHome, replacing
// any file left by an earlier start, and returns the task to pass to the
// harness: task itself when it is at most InlineTaskMaxBytes, otherwise a
// short task that points to the file. An empty task writes nothing and
// is returned unchanged.
func deliverTaskFile(agentHome, task string) (string, error) {
	if task == "" || agentHome == "" {
		return task, nil
	}
	path := filepath.Join(agentHome, filepath.FromSlash(TaskFileRel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("failed to create the task file directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(task), 0o644); err != nil {
		return "", fmt.Errorf("failed to write the task file %s: %w", path, err)
	}
	// os.WriteFile keeps the mode of an existing file; set it explicitly.
	if err := os.Chmod(path, 0o644); err != nil {
		return "", fmt.Errorf("failed to set the mode of the task file %s: %w", path, err)
	}
	if len(task) <= InlineTaskMaxBytes {
		return task, nil
	}
	return taskFilePointer(len(task)), nil
}

// taskFilePointer is the task given to the harness in place of a task of
// size bytes that is too large to pass inline.
func taskFilePointer(size int) string {
	return fmt.Sprintf("Your task is in the file %s (%d bytes). "+
		"Read the whole file before you start, then carry out the task it describes.",
		TaskFileContainerPath, size)
}
