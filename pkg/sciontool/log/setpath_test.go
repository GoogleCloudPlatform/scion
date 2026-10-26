/*
Copyright 2026 The Scion Authors.
*/

package log

import "testing"

// setLogPathForTest points the package-level log path at path for the
// duration of a test and restores the previous state afterward.
func setLogPathForTest(t *testing.T, path string) func() {
	t.Helper()
	mu.Lock()
	origPath := logPath
	origFile := logFile
	origInitialized := initialized.Load()
	logFile = nil
	mu.Unlock()

	SetLogPath(path)

	return func() {
		mu.Lock()
		if logFile != nil {
			_ = logFile.Close()
		}
		logPath = origPath
		logFile = origFile
		initialized.Store(origInitialized)
		mu.Unlock()
	}
}
