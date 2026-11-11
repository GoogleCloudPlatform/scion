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

//go:build unix

package cmd

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSignalWaitContext_RecordsSignal(t *testing.T) {
	ctx, stop := signalWaitContext()
	defer stop()
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context not cancelled by SIGTERM")
	}
	assert.Equal(t, syscall.SIGTERM, waitSignalOf(ctx))
}

func TestSignalWaitContext_StopCancelsWithoutSignal(t *testing.T) {
	ctx, stop := signalWaitContext()
	stop()
	<-ctx.Done()
	assert.Nil(t, waitSignalOf(ctx))
}
