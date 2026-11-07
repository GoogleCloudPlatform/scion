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

// Command prodprobe is built and run, as an ordinary (non-test) binary, by
// TestAllowLocalCloneFixture_NotHonouredOutsideTestBinary. It registers a
// local clone fixture and asks ProvisionShared to clone it, then prints
// whether the binary is a test binary, whether testing.Init ran, and
// whether the clone URL was refused.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: prodprobe <local repository> <workspace>")
		os.Exit(2)
	}
	src, workspace := os.Args[1], os.Args[2]
	provision.AllowLocalCloneFixture(&testing.T{}, src)
	err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved:    provision.ResolvedWorkspace{HostPath: workspace},
		ProjectID:   "prodprobe",
		Mode:        store.SharingModeSharedPlain,
		SentinelDir: workspace,
		GitClone:    &api.GitCloneConfig{URL: src},
	})
	fmt.Printf("testing=%t init=%t refused=%t\n",
		testing.Testing(), flag.Lookup("test.v") != nil, errors.Is(err, provision.ErrHostCloneURLRefused))
}
