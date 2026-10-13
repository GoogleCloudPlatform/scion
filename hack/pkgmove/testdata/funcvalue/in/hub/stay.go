package hub

import "reflect"

// IsDefault compares func identity, as hook-restoring tests do.
func IsDefault() bool {
	return reflect.ValueOf(hook).Pointer() == reflect.ValueOf(defaultHook).Pointer()
}

// Run calls the hook's default directly.
func Run() string { return defaultHook() }
