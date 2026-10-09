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

package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// taskFilePath is the --task-file flag of start and create.
var taskFilePath string

// maxTaskFileBytes is the largest task --task-file accepts, counting any
// task given as arguments. It leaves room for JSON escaping within the
// request size the Hub and its broker connection carry.
const maxTaskFileBytes = 128 * 1024

const taskFileFlagUsage = "Read the task from a file ('-' for stdin), up to 128 KiB. " +
	"Task arguments, if any, come first. The full task is written to ~/.scion/task.md in the agent"

// errTaskFileTooLarge reports a task over maxTaskFileBytes.
var errTaskFileTooLarge = errors.New("task is over the 128 KiB (131072 bytes) limit for --task-file")

// applyTaskFile returns task with the content of the file at path
// appended, separated by a blank line, or task unchanged when path is
// empty. path "-" reads stdin. The file must be valid UTF-8 and not
// empty, and the result must be at most maxTaskFileBytes.
func applyTaskFile(task, path string, stdin io.Reader) (string, error) {
	if path == "" {
		return task, nil
	}
	name := path
	var r io.Reader
	if path == "-" {
		name = "stdin"
		r = stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("--task-file: %w", err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, maxTaskFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("--task-file: failed to read %s: %w", name, err)
	}
	if len(data) > maxTaskFileBytes {
		return "", fmt.Errorf("--task-file %s: %w", name, errTaskFileTooLarge)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("--task-file %s: the file is not valid UTF-8 text", name)
	}
	content := string(data)
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("--task-file %s: the file is empty", name)
	}
	if task = strings.TrimSpace(task); task != "" {
		content = task + "\n\n" + content
	}
	if len(content) > maxTaskFileBytes {
		return "", fmt.Errorf("--task-file %s with the task arguments is %d bytes: %w", name, len(content), errTaskFileTooLarge)
	}
	return content, nil
}

// validateTaskFileStdin rejects reading both --task-file and --config
// from stdin.
func validateTaskFileStdin() error {
	if taskFilePath == "-" && inlineConfigPath == "-" {
		return newUsageError("--task-file and --config cannot both read from stdin ('-')")
	}
	return nil
}
