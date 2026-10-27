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

//go:build !unix

package util

import (
	"fmt"
	"io"
	"os"
)

// openForLstatVerify is the open step of the non-unix regular-file readers.
// It is a variable so a test can make it return a different file than the
// one at path.
var openForLstatVerify = os.Open

// readSmallRegularFile reads the file at path when it is a regular file of
// at most max bytes. A symlink or non-regular file is refused with Lstat
// before opening, and the opened file is then checked by openLstatVerified,
// whose comment says what that identity check guarantees on each platform.
func readSmallRegularFile(path string, max int64) ([]byte, error) {
	f, err := openLstatVerified(path, openForLstatVerify)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	return data, nil
}

// openRegularFileNoFollow opens path read-only. A symlink or non-regular file
// is refused with Lstat before opening, and the opened file is then checked
// by openLstatVerified, whose comment says what that identity check
// guarantees on each platform.
func openRegularFileNoFollow(path string) (*os.File, error) {
	return openLstatVerified(path, openForLstatVerify)
}
