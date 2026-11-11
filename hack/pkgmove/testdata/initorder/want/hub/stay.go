package hub

import "os"

// setErr sets the environment during package initialisation.
var setErr = os.Setenv("FX_HOME", "/configured")

// Configured reports the init-time result.
func Configured() (string, error) { return Home(), setErr }
