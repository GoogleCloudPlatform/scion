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
	"io/fs"
	"os"
)

// Error classes for filesystem warnings. Log lines carry one of these fixed
// tokens instead of the error itself, so they hold no filesystem path.
const (
	fsErrorClassNotExist   = "not_exist"
	fsErrorClassPermission = "permission"
	fsErrorClassTimeout    = "timeout"
	fsErrorClassOther      = "other"
)

// fsErrorClass maps err to one of the fsErrorClass* tokens. It only inspects
// err with errors.Is and never formats it.
func fsErrorClass(err error) string {
	switch {
	case errors.Is(err, errWorkspaceContentTimeout),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, os.ErrDeadlineExceeded):
		return fsErrorClassTimeout
	case errors.Is(err, fs.ErrNotExist):
		return fsErrorClassNotExist
	case errors.Is(err, fs.ErrPermission):
		return fsErrorClassPermission
	default:
		return fsErrorClassOther
	}
}
