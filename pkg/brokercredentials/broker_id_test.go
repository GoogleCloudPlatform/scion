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

package brokercredentials

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// notPathElementIDs are broker IDs that are not a single path element.
var notPathElementIDs = map[string]string{
	"empty":            "",
	"dot":              ".",
	"dot dot":          "..",
	"slash":            "/",
	"absolute":         "/abs",
	"nested":           "a/b",
	"parent prefix":    "../x",
	"parent suffix":    "x/..",
	"inner dot dot":    "a..b",
	"backslash":        `a\b`,
	"nul":              "a\x00b",
	"trailing slash":   "a/",
	"backslash parent": `..\x`,
}

func TestValidateBrokerIDPathElement(t *testing.T) {
	for name, id := range notPathElementIDs {
		t.Run(name, func(t *testing.T) {
			err := ValidateBrokerIDPathElement(id)
			require.Error(t, err, "broker ID %q accepted as a path element", id)
			assert.True(t, errors.Is(err, ErrBrokerIDNotPathElement))
		})
	}
	for _, id := range []string{"11111111-2222-3333-4444-555555555555", "default", "b-1", "broker_2.local"} {
		assert.NoError(t, ValidateBrokerIDPathElement(id), id)
	}
}

// TestSave_RejectsBrokerIDNotPathElement: the store does not write
// credentials whose broker ID is not a single path element.
func TestSave_RejectsBrokerIDNotPathElement(t *testing.T) {
	for name, id := range notPathElementIDs {
		if id == "" {
			continue // "brokerId is required"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			ms := NewMultiStore(filepath.Join(dir, "multi"))
			err := ms.Save(&BrokerCredentials{Name: "hub-a", BrokerID: id, SecretKey: "c2VjcmV0"})
			require.Error(t, err, "MultiStore.Save accepted broker ID %q", id)
			assert.True(t, errors.Is(err, ErrBrokerIDNotPathElement))
			_, statErr := os.Stat(filepath.Join(dir, "multi", "hub-a.json"))
			assert.True(t, os.IsNotExist(statErr), "multistore credentials file written")
		})
	}
	dir := t.TempDir()
	require.NoError(t, NewMultiStore(filepath.Join(dir, "m")).Save(&BrokerCredentials{Name: "hub-a", BrokerID: "11111111-2222-3333-4444-555555555555", SecretKey: "c2VjcmV0"}))
}
