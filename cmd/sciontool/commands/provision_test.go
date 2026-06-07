/*
Copyright 2026 The Scion Authors.
*/
package commands

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProvisionCmd_WaitForSentinel_Found(t *testing.T) {
	dir := t.TempDir()

	sentinelPath := filepath.Join(dir, ".scion-provisioned")
	if err := os.WriteFile(sentinelPath, []byte("provisioned_at=test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	oldWorkspace := provisionWorkspace
	oldWait := provisionWaitSentinel
	oldTimeout := provisionTimeout
	oldInterval := provisionPollInterval
	defer func() {
		provisionWorkspace = oldWorkspace
		provisionWaitSentinel = oldWait
		provisionTimeout = oldTimeout
		provisionPollInterval = oldInterval
	}()

	provisionWorkspace = dir
	provisionWaitSentinel = true
	provisionTimeout = 5
	provisionPollInterval = 1

	if err := runWaitForSentinel(context.Background()); err != nil {
		t.Fatalf("expected success when sentinel exists, got: %v", err)
	}
}

func TestProvisionCmd_WaitForSentinel_Timeout(t *testing.T) {
	dir := t.TempDir()

	oldWorkspace := provisionWorkspace
	oldWait := provisionWaitSentinel
	oldTimeout := provisionTimeout
	oldInterval := provisionPollInterval
	defer func() {
		provisionWorkspace = oldWorkspace
		provisionWaitSentinel = oldWait
		provisionTimeout = oldTimeout
		provisionPollInterval = oldInterval
	}()

	provisionWorkspace = dir
	provisionWaitSentinel = true
	provisionTimeout = 3
	provisionPollInterval = 1

	start := time.Now()
	err := runWaitForSentinel(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error when sentinel is missing")
	}
	if elapsed < 2*time.Second {
		t.Errorf("should have waited at least 2s, only waited %s", elapsed)
	}
}

func TestProvisionCmd_WaitForSentinel_DelayedWrite(t *testing.T) {
	dir := t.TempDir()

	oldWorkspace := provisionWorkspace
	oldWait := provisionWaitSentinel
	oldTimeout := provisionTimeout
	oldInterval := provisionPollInterval
	defer func() {
		provisionWorkspace = oldWorkspace
		provisionWaitSentinel = oldWait
		provisionTimeout = oldTimeout
		provisionPollInterval = oldInterval
	}()

	provisionWorkspace = dir
	provisionWaitSentinel = true
	provisionTimeout = 10
	provisionPollInterval = 1

	go func() {
		time.Sleep(2 * time.Second)
		sentinelPath := filepath.Join(dir, ".scion-provisioned")
		_ = os.WriteFile(sentinelPath, []byte("provisioned_at=test\n"), 0644)
	}()

	if err := runWaitForSentinel(context.Background()); err != nil {
		t.Fatalf("expected success after delayed sentinel write, got: %v", err)
	}
}

func TestProvisionCmd_WaitForSentinel_ContextCancel(t *testing.T) {
	dir := t.TempDir()

	oldWorkspace := provisionWorkspace
	oldWait := provisionWaitSentinel
	oldTimeout := provisionTimeout
	oldInterval := provisionPollInterval
	defer func() {
		provisionWorkspace = oldWorkspace
		provisionWaitSentinel = oldWait
		provisionTimeout = oldTimeout
		provisionPollInterval = oldInterval
	}()

	provisionWorkspace = dir
	provisionWaitSentinel = true
	// Long timeout/interval: the loop would block well past the test budget if
	// cancellation were not honoured.
	provisionTimeout = 60
	provisionPollInterval = 30

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := runWaitForSentinel(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
	if elapsed > 5*time.Second {
		t.Errorf("cancellation should interrupt the poll sleep promptly, waited %s", elapsed)
	}
}

func TestProvisionCmd_Clone_Idempotent(t *testing.T) {
	dir := t.TempDir()
	wsDir := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wsDir, 0770); err != nil {
		t.Fatal(err)
	}

	sentinelPath := filepath.Join(wsDir, ".scion-provisioned")
	if err := os.WriteFile(sentinelPath, []byte("provisioned_at=test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	oldWorkspace := provisionWorkspace
	oldMode := provisionMode
	oldDepth := provisionDepth
	oldUID := provisionUID
	oldGID := provisionGID
	defer func() {
		provisionWorkspace = oldWorkspace
		provisionMode = oldMode
		provisionDepth = oldDepth
		provisionUID = oldUID
		provisionGID = oldGID
	}()

	provisionWorkspace = wsDir
	provisionMode = "shared-plain"
	provisionDepth = 1
	provisionUID = os.Getuid()
	provisionGID = os.Getgid()

	t.Setenv("SCION_CLONE_URL", "https://nonexistent.example.com/repo.git")
	t.Setenv("SCION_CLONE_BRANCH", "main")
	t.Setenv("SCION_PROJECT_ID", "test-proj")

	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("idempotent provision (sentinel exists) should succeed, got: %v", err)
	}
}

func TestProvisionCmd_Clone_NoURL(t *testing.T) {
	dir := t.TempDir()

	oldWorkspace := provisionWorkspace
	oldMode := provisionMode
	oldDepth := provisionDepth
	oldUID := provisionUID
	oldGID := provisionGID
	defer func() {
		provisionWorkspace = oldWorkspace
		provisionMode = oldMode
		provisionDepth = oldDepth
		provisionUID = oldUID
		provisionGID = oldGID
	}()

	provisionWorkspace = dir
	provisionMode = "shared-plain"
	provisionDepth = 1
	provisionUID = os.Getuid()
	provisionGID = os.Getgid()

	t.Setenv("SCION_CLONE_URL", "")
	t.Setenv("SCION_CLONE_BRANCH", "")
	t.Setenv("SCION_PROJECT_ID", "test-proj-no-url")

	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("provision without clone URL should succeed (non-git project), got: %v", err)
	}

	sentinelPath := filepath.Join(dir, ".scion-provisioned")
	if _, err := os.Stat(sentinelPath); err != nil {
		t.Errorf("sentinel should be written for non-git project: %v", err)
	}
}
