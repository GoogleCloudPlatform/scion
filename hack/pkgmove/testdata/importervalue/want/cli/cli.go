package cli

import "example.com/fx/hub"

func Check() bool { return hub.IsDefault(hub.DefaultHook) }
