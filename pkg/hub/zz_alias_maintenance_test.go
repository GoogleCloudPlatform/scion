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

// Aliases for symbols moved to
// github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance
// by hack/pkgmove, so existing references in package hub keep compiling.

import (
	"context"
	"io"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func backupBinary(p0 context.Context, p1 string, p2 string, p3 io.Writer) error {
	return maintenance.BackupBinary(p0, p1, p2, p3)
}

func deriveChecksumsURL(p0 string) string {
	return maintenance.DeriveChecksumsURL(p0)
}

func deriveImageBaseName(p0 *store.HarnessConfig) string {
	return maintenance.DeriveImageBaseName(p0)
}

func downloadFile(p0 context.Context, p1 string, p2 string, p3 io.Writer) error {
	return maintenance.DownloadFile(p0, p1, p2, p3)
}

func extractScionBinary(p0 string, p1 string, p2 io.Writer) (string, error) {
	return maintenance.ExtractScionBinary(p0, p1, p2)
}

func fetchExpectedChecksum(p0 context.Context, p1 string, p2 string, p3 io.Writer) (string, error) {
	return maintenance.FetchExpectedChecksum(p0, p1, p2, p3)
}

func restoreBackup(p0 string, p1 string, p2 io.Writer) error {
	return maintenance.RestoreBackup(p0, p1, p2)
}

func syncBuiltImage(p0 context.Context, p1 store.Store, p2 io.Writer, p3 *store.HarnessConfig, p4 string) error {
	return maintenance.SyncBuiltImage(p0, p1, p2, p3, p4)
}

func verifyFileChecksum(p0 string, p1 string, p2 string, p3 io.Writer) error {
	return maintenance.VerifyFileChecksum(p0, p1, p2, p3)
}

func verifyScionBinary(p0 context.Context, p1 string, p2 string, p3 io.Writer) error {
	return maintenance.VerifyScionBinary(p0, p1, p2, p3)
}
