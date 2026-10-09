/*
Copyright 2026 The Scion Authors.
*/

package log

import (
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"
)

// TestMain clears the log level variables so tests that read agent.log do
// not depend on the caller's SCION_LOG_LEVEL / SCION_DEBUG.
func TestMain(m *testing.M) {
	_ = os.Unsetenv(loglevel.EnvLogLevel)
	_ = os.Unsetenv(loglevel.EnvDebug)
	os.Exit(m.Run())
}
