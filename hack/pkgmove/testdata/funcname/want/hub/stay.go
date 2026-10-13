package hub

import (
	"reflect"
	"runtime"

	"example.com/fx/hub/sub"
)

var hook = sub.DefaultHook
var methodExpr = T.Run

func HookName() string { return runtime.FuncForPC(reflect.ValueOf(hook).Pointer()).Name() }
func IsDefault() bool {
	return reflect.ValueOf(hook).Pointer() == reflect.ValueOf(sub.DefaultHook).Pointer()
}
func Run() string { return methodExpr(T{}) + defaultHook() }
