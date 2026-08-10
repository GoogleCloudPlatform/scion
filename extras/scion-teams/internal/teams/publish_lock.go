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

package teams

import (
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/integration/lockloop"
)

// AdvisoryLocker is the subset of Store needed by PublishLockLoop.
type AdvisoryLocker = lockloop.AdvisoryLocker

// PublishLockLoop is an alias for the shared lock loop implementation.
type PublishLockLoop = lockloop.LockLoop

// NewPublishLockLoop creates a lock loop for Teams outbound publish singleton.
// Configure OnAcquired and OnLost before calling Run.
func NewPublishLockLoop(locker AdvisoryLocker, lockKey int64, log *slog.Logger) *PublishLockLoop {
	return lockloop.New(locker, lockKey, log)
}
