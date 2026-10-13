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

package sshd

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
)

// HelperCommand is the hidden sciontool helper subcommand (unix only).
const HelperCommand = "conduit-ssh-helper"

const helperSFTP = "sftp-server"

func helperDialer(string, User, func() ([]string, error)) func(context.Context, string) (net.Conn, error) {
	return func(context.Context, string) (net.Conn, error) { return nil, errUnsupported }
}

// RunHelper is unsupported on this platform.
func RunHelper([]string, io.Reader, io.WriteCloser) int {
	fmt.Fprintln(os.Stderr, errUnsupported)
	return 2
}
