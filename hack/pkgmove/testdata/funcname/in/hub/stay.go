package hub

import (
	"reflect"
	"runtime"
)

var hook = defaultHook
var methodExpr = T.run

func HookName() string { return runtime.FuncForPC(reflect.ValueOf(hook).Pointer()).Name() }
func IsDefault() bool {
	return reflect.ValueOf(hook).Pointer() == reflect.ValueOf(defaultHook).Pointer()
}
func Run() string { return methodExpr(T{}) + defaultHook() }
