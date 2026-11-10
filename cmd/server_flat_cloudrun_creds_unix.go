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

package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// openFlatCredentialsFile is precondition P6 (amendment P5-CR): it opens a
// flat cloudrun instance's credentials file without following a symlink
// (O_NOFOLLOW; O_NONBLOCK so a FIFO cannot block the open) and checks the
// opened handle, never a path stat: a regular file, owned by the service
// user and private (privatefs.CheckPrivate), with mode 0600 or 0400. The
// bytes are read from that same handle (CR3 credentials, checks P6 and P7),
// so a file swapped at the path
// after the check is never the one parsed. Errors never name the path.
func openFlatCredentialsFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		switch {
		case errors.Is(err, syscall.ELOOP):
			return nil, errors.New("credentials_file must not be a symlink")
		case errors.Is(err, os.ErrNotExist):
			return nil, errors.New("credentials_file does not exist")
		case errors.Is(err, os.ErrPermission):
			return nil, errors.New("credentials_file cannot be read by the service user")
		default:
			return nil, errors.New("credentials_file cannot be opened")
		}
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, errors.New("credentials_file cannot be inspected")
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("credentials_file must be a regular file")
	}
	if err := privatefs.CheckPrivate(f); err != nil {
		return nil, errors.New("credentials_file must be a private regular file owned by the service user (0600 or 0400)")
	}
	if perm := fi.Mode().Perm(); perm != 0o600 && perm != 0o400 {
		return nil, fmt.Errorf("credentials_file must have mode 0600 or 0400, not %04o", perm)
	}
	data, err := io.ReadAll(io.LimitReader(f, flatCredentialsMaxBytes+1))
	if err != nil {
		return nil, errors.New("credentials_file cannot be read")
	}
	if len(data) > flatCredentialsMaxBytes {
		return nil, errors.New("credentials_file is too large to be a credential file")
	}
	return data, nil
}
