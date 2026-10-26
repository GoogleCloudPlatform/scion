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

package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// The project storage migration engine (pkg/projectfs/migrate) walks and
// moves trees on descriptors (openat, O_NOFOLLOW, fchown) and builds only
// on unix. The hub server never runs on this platform, so the operation and
// its notice plan fail with errors.ErrUnsupported. See
// project_storage_migrate_unix.go.

// Run always fails with errors.ErrUnsupported on this platform.
func (e *ProjectStorageMigrateExecutor) Run(ctx context.Context, w io.Writer, params map[string]string) error {
	return fmt.Errorf("project storage migration: %w on this platform", errors.ErrUnsupported)
}

// planStorageMigration always fails with errors.ErrUnsupported on this
// platform.
func (s *Server) planStorageMigration(ctx context.Context) (map[string]string, error) {
	return nil, fmt.Errorf("project storage migration plan: %w on this platform", errors.ErrUnsupported)
}
