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

package store

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdleSuspendPreconditionError(t *testing.T) {
	err := &IdleSuspendPreconditionError{
		Predicate: "delivery_since_activity",
		Attrs:     []any{"key", "val"},
	}
	assert.ErrorIs(t, err, ErrIdleSuspendPrecondition)
	assert.ErrorIs(t, err, ErrVersionConflict)
	assert.Equal(t, ErrIdleSuspendPrecondition.Error()+" (delivery_since_activity)", err.Error())

	var extracted *IdleSuspendPreconditionError
	require.True(t, errors.As(err, &extracted))
	assert.Equal(t, "delivery_since_activity", extracted.Predicate)

	empty := &IdleSuspendPreconditionError{}
	assert.Equal(t, ErrIdleSuspendPrecondition.Error(), empty.Error())
	assert.ErrorIs(t, empty, ErrIdleSuspendPrecondition)

	var nilErr *IdleSuspendPreconditionError
	assert.Equal(t, ErrIdleSuspendPrecondition.Error(), nilErr.Error())
}
