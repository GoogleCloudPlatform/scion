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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubGCPMetadata replaces the ResolveGCPProjectID seams for one test and
// counts how often each is called.
func stubGCPMetadata(t *testing.T, onGCE bool, projectID func(context.Context) (string, error)) (onGCECalls, lookupCalls *int) {
	t.Helper()
	origOnGCE, origProjectID := gcpOnGCE, gcpMetadataProjectID
	t.Cleanup(func() { gcpOnGCE, gcpMetadataProjectID = origOnGCE, origProjectID })
	onGCECalls, lookupCalls = new(int), new(int)
	gcpOnGCE = func() bool { *onGCECalls++; return onGCE }
	gcpMetadataProjectID = func(ctx context.Context) (string, error) {
		*lookupCalls++
		return projectID(ctx)
	}
	return onGCECalls, lookupCalls
}

func TestResolveGCPProjectID_ConfiguredWinsWithoutLookup(t *testing.T) {
	onGCECalls, lookupCalls := stubGCPMetadata(t, true, func(context.Context) (string, error) {
		return "detected-project", nil
	})

	got, err := ResolveGCPProjectID(context.Background(), "configured-project")
	require.NoError(t, err)
	assert.Equal(t, "configured-project", got)
	assert.Zero(t, *onGCECalls, "a configured project ID must not probe for GCP")
	assert.Zero(t, *lookupCalls, "a configured project ID must not query the metadata server")
}

func TestResolveGCPProjectID_OffGCPSkipsLookup(t *testing.T) {
	_, lookupCalls := stubGCPMetadata(t, false, func(ctx context.Context) (string, error) {
		// An unreachable metadata server: the lookup would block until
		// its deadline.
		<-ctx.Done()
		return "", ctx.Err()
	})

	start := time.Now()
	got, err := ResolveGCPProjectID(context.Background(), "")
	elapsed := time.Since(start)

	require.ErrorIs(t, err, errNotOnGCP)
	assert.Contains(t, err.Error(), "server.hub.gcp_project_id")
	assert.Empty(t, got)
	assert.Zero(t, *lookupCalls, "off GCP the metadata server must not be queried")
	assert.Less(t, elapsed, time.Second, "off GCP resolution must not wait on the metadata server")
}

func TestResolveGCPProjectID_OnGCPAutoDetects(t *testing.T) {
	var hadDeadline bool
	_, lookupCalls := stubGCPMetadata(t, true, func(ctx context.Context) (string, error) {
		deadline, ok := ctx.Deadline()
		hadDeadline = ok && time.Until(deadline) <= gcpProjectIDLookupTimeout
		return "detected-project", nil
	})

	got, err := ResolveGCPProjectID(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, "detected-project", got)
	assert.Equal(t, 1, *lookupCalls)
	assert.True(t, hadDeadline, "the metadata lookup must run under gcpProjectIDLookupTimeout")
}

func TestResolveGCPProjectID_LookupErrors(t *testing.T) {
	t.Run("lookup error", func(t *testing.T) {
		boom := errors.New("boom")
		stubGCPMetadata(t, true, func(context.Context) (string, error) { return "", boom })
		_, err := ResolveGCPProjectID(context.Background(), "")
		require.ErrorIs(t, err, boom)
		assert.Contains(t, err.Error(), "auto-detecting GCP project ID")
	})
	t.Run("empty project ID", func(t *testing.T) {
		stubGCPMetadata(t, true, func(context.Context) (string, error) { return "", nil })
		_, err := ResolveGCPProjectID(context.Background(), "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty project ID")
	})
	t.Run("caller cancellation is honoured", func(t *testing.T) {
		stubGCPMetadata(t, true, func(ctx context.Context) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := ResolveGCPProjectID(ctx, "")
		require.ErrorIs(t, err, context.Canceled)
	})
}
