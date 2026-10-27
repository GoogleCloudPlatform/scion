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

package runtimebroker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockJournalInstance takes the exclusive flock on <root>/<id>.lock without
// blocking. It retries when the lock file was replaced between open and
// flock (a recovery removing a dead instance's lock), so the lock it returns
// is always on the file at the path.
func lockJournalInstance(root, instanceID string, mustExist bool) (*os.File, error) {
	path := filepath.Join(root, instanceID+".lock")
	for attempt := 0; attempt < 5; attempt++ {
		flags := os.O_RDWR
		if !mustExist {
			flags |= os.O_CREATE
		}
		f, err := os.OpenFile(path, flags, journalFileMode)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, errJournalLocked
			}
			return nil, err
		}
		held, herr := f.Stat()
		cur, cerr := os.Stat(path)
		if herr == nil && cerr == nil && os.SameFile(held, cur) {
			return f, nil
		}
		unlockJournalInstance(f)
		if mustExist && errors.Is(cerr, os.ErrNotExist) {
			return nil, cerr
		}
	}
	return nil, fmt.Errorf("launch journal lock %s kept changing", path)
}

func unlockJournalInstance(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
