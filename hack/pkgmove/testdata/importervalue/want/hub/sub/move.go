package sub

func DefaultHook() string { return "d" }

func IsDefault(f func() string) bool {
	return reflectPtr(f) == reflectPtr(DefaultHook)
}
