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

package discord

import (
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/integration/lockloop"
)

// AdvisoryLocker is the subset of Store needed by GatewayLockLoop.
type AdvisoryLocker = lockloop.AdvisoryLocker

// GatewayLockLoop is an alias for the shared lock loop implementation.
type GatewayLockLoop = lockloop.LockLoop

// NewGatewayLockLoop creates a lock loop for the Discord Gateway.
// Configure OnAcquired and OnLost before calling Run.
func NewGatewayLockLoop(locker AdvisoryLocker, lockKey int64, log *slog.Logger) *GatewayLockLoop {
	return lockloop.New(locker, lockKey, log)
}
