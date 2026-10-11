package hub

import (
	"reflect"

	"example.com/fx/hub/sub"
)

// IsDefault compares func identity, as hook-restoring tests do.
func IsDefault() bool {
	return reflect.ValueOf(sub.Hook).Pointer() == reflect.ValueOf(sub.DefaultHook).Pointer()
}

// Run calls the hook's default directly.
func Run() string { return defaultHook() }
