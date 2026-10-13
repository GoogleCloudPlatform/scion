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

package main

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
)

// maxRSSBytes converts ru_maxrss from the wait4 rusage to bytes. Linux and
// most BSDs report KiB; Darwin reports bytes.
func maxRSSBytes(ps *os.ProcessState) int64 {
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return 0
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
		return int64(ru.Maxrss)
	}
	return int64(ru.Maxrss) * 1024
}

func rlimitAS() string {
	var l syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_AS, &l); err != nil {
		return ""
	}
	if l.Cur == ^uint64(0) {
		return "unlimited"
	}
	return fmt.Sprintf("%d KiB", l.Cur/1024)
}
