//go:build !no_sqlite

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

package hub

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// downloadPathStorage records each Download path and fails it, so a build
// stops as soon as it starts reading harness-config files.
type downloadPathStorage struct {
	storage.Storage
	paths []string
}

func (s *downloadPathStorage) Download(_ context.Context, objectPath string) (io.ReadCloser, *storage.Object, error) {
	s.paths = append(s.paths, objectPath)
	return nil, nil, errors.New("download stopped by test")
}

// TestResolveBuildHarnessConfigImage_WiresObjectPath: the
// build-harness-config-image executor the hub resolves reads each file at
// harnessConfigFileObjectPath (its required ObjectPath option).
func TestResolveBuildHarnessConfigImage_WiresObjectPath(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	stor := &downloadPathStorage{}
	srv.SetStorage(stor)
	hc := &store.HarnessConfig{
		ID: tid("objpath-hc"), Name: "objpath", Slug: "objpath", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		StoragePath: "harness-configs/objpath",
		Files:       []store.TemplateFile{{Path: "Dockerfile"}},
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, hc))
	got, err := s.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	require.Len(t, got.Files, 1)

	exec, err := srv.resolveMaintenanceExecutor("build-harness-config-image")
	require.NoError(t, err)
	err = exec.Run(ctx, io.Discard, map[string]string{"harness_config_id": hc.ID})
	require.ErrorContains(t, err, "download stopped by test")
	require.Equal(t, []string{harnessConfigFileObjectPath(got, got.Files[0])}, stor.paths)
}
