package sub

import "reflect"

func reflectPtr(f func() string) uintptr { return reflect.ValueOf(f).Pointer() }
