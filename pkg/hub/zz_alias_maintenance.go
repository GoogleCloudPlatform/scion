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
	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
)

type (
	BinaryUpdateExecutor             = maintenance.BinaryUpdateExecutor
	BinaryUpdateOptions              = maintenance.BinaryUpdateOptions
	BuildHarnessConfigImageExecutor  = maintenance.BuildHarnessConfigImageExecutor
	BuildHarnessConfigImageOptions   = maintenance.BuildHarnessConfigImageOptions
	CloudBuildHarnessConfigExecutor  = maintenance.CloudBuildHarnessConfigExecutor
	MaintenanceExecutor              = maintenance.MaintenanceExecutor
	PullImagesExecutor               = maintenance.PullImagesExecutor
	PullImagesOptions                = maintenance.PullImagesOptions
	RebuildContainerBinariesExecutor = maintenance.RebuildContainerBinariesExecutor
	RebuildServerExecutor            = maintenance.RebuildServerExecutor
	RebuildServerOptions             = maintenance.RebuildServerOptions
	RebuildWebExecutor               = maintenance.RebuildWebExecutor
	RebuildWebOptions                = maintenance.RebuildWebOptions
	SecretMigrationExecutor          = maintenance.SecretMigrationExecutor
	SecretMigrationResult            = maintenance.SecretMigrationResult
	UnitDriftStatus                  = maintenance.UnitDriftStatus
	UpdateCheckResult                = maintenance.UpdateCheckResult
	UpdateCommitInfo                 = maintenance.UpdateCommitInfo
)

const (
	HubSettingSectionUnitDrift       = maintenance.HubSettingSectionUnitDrift
	HubSettingSectionUpdateAvailable = maintenance.HubSettingSectionUpdateAvailable
)

var (
	CheckForUpdates                     = maintenance.CheckForUpdates
	NewBinaryUpdateExecutor             = maintenance.NewBinaryUpdateExecutor
	NewBuildHarnessConfigImageExecutor  = maintenance.NewBuildHarnessConfigImageExecutor
	NewPullImagesExecutor               = maintenance.NewPullImagesExecutor
	NewRebuildContainerBinariesExecutor = maintenance.NewRebuildContainerBinariesExecutor
	NewRebuildServerExecutor            = maintenance.NewRebuildServerExecutor
	NewRebuildWebExecutor               = maintenance.NewRebuildWebExecutor
	NewSecretMigrationExecutor          = maintenance.NewSecretMigrationExecutor
)

func parseMigrationParams(p0 map[string]interface{}) map[string]string {
	return maintenance.ParseMigrationParams(p0)
}
