// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logging

import (
	"context"
	"encoding"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/cloudrunenv"
)

var (
	timeType          = reflect.TypeFor[time.Time]()
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

type visitKey struct {
	ptr uintptr
	typ reflect.Type
}

// sanitizedErrorText is the one way a log sink renders an error-valued
// attribute: the error's text through SanitizeText. fmt.Sprint guards an
// Error method that panics, such as one on a nil pointer receiver.
func sanitizedErrorText(err error) string {
	return SanitizeText(fmt.Sprint(err))
}

// validCloudTime reports whether t encodes cleanly as strict RFC 3339 (matching
// encoding/json's Time.MarshalJSON check on year [0, 9999] and zone offset).
func validCloudTime(t time.Time) bool {
	_, err := t.MarshalText()
	return err == nil
}

// cloudAttrValue returns the Cloud Logging payload value for v, a resolved
// value that is not a group: an error as its sanitized text, any value (or
// nested element) that encoding/json rejects replaced with a string while
// keeping surrounding composite values intact, and valid values unchanged.
func cloudAttrValue(v slog.Value) any {
	switch v.Kind() {
	case slog.KindString, slog.KindInt64, slog.KindUint64, slog.KindBool, slog.KindDuration:
		return v.Any()
	case slog.KindFloat64:
		f := v.Float64()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return strconv.FormatFloat(f, 'g', -1, 64)
		}
		return f
	case slog.KindTime:
		t := v.Time()
		if !validCloudTime(t) {
			return SanitizeText(t.Round(0).String())
		}
		return t
	}
	a := v.Any()
	if a == nil {
		return nil
	}
	if err, ok := a.(error); ok {
		return sanitizedErrorText(err)
	}
	switch a.(type) {
	case string, bool, []string, map[string]string,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, uintptr:
		return a // always encodes; skip the trial encoding below
	}
	if validCloudPayloadValue(a) {
		return a
	}
	repaired := sanitizeCloudReflect(reflect.ValueOf(a), make(map[visitKey]bool))
	if _, err := tryJSONMarshal(repaired); err != nil {
		return SanitizeText(fmt.Sprintf("!ERROR:%v", err))
	}
	return repaired
}

func tryJSONMarshal(v any) (raw []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return json.Marshal(v)
}

func tryMarshalText(tm encoding.TextMarshaler) (raw []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return tm.MarshalText()
}

func validCloudPayloadValue(a any) bool {
	_, err := tryJSONMarshal(a)
	return err == nil
}

// sanitizeCloudReflect clones only the composite path leading to unencodable
// values, leaving valid subtrees untouched and omitting unexported and
// json:"-" struct fields. It runs only after validCloudPayloadValue failed on
// the outer attribute, and re-tests each subtree with validCloudPayloadValue
// (O(n*depth) on already-broken values) so valid siblings and cycles hidden
// behind custom marshalers pass through without descent.
func sanitizeCloudReflect(rv reflect.Value, visiting map[visitKey]bool) any {
	for rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return nil
	}
	if rv.Type() == timeType && rv.CanInterface() {
		t := rv.Interface().(time.Time)
		if !validCloudTime(t) {
			return SanitizeText(t.Round(0).String())
		}
		return t
	}
	if rv.Kind() == reflect.Pointer && rv.IsNil() {
		return nil
	}
	if rv.Kind() != reflect.Pointer && rv.CanAddr() && rv.CanInterface() {
		pt := reflect.PointerTo(rv.Type())
		if pt.Implements(jsonMarshalerType) || pt.Implements(textMarshalerType) {
			pv := rv.Addr().Interface()
			if validCloudPayloadValue(pv) {
				return pv
			}
			if errVal, ok := pv.(error); ok {
				return sanitizedErrorText(errVal)
			}
			_, err := tryJSONMarshal(pv)
			return SanitizeText(fmt.Sprintf("!ERROR:%v", err))
		}
	}
	if rv.CanInterface() && validCloudPayloadValue(rv.Interface()) {
		return rv.Interface()
	}
	if rv.Type().Implements(jsonMarshalerType) || rv.Type().Implements(textMarshalerType) {
		if rv.CanInterface() {
			v := rv.Interface()
			if errVal, ok := v.(error); ok {
				return sanitizedErrorText(errVal)
			}
			_, err := tryJSONMarshal(v)
			return SanitizeText(fmt.Sprintf("!ERROR:%v", err))
		}
	}

	switch rv.Kind() {
	case reflect.Pointer:
		vk := visitKey{ptr: rv.Pointer(), typ: rv.Type()}
		if visiting[vk] {
			return "<cycle>"
		}
		visiting[vk] = true
		defer delete(visiting, vk)
		return sanitizeCloudReflect(rv.Elem(), visiting)
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		bits := 64
		if rv.Kind() == reflect.Float32 {
			bits = 32
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return strconv.FormatFloat(f, 'g', -1, bits)
		}
		return f
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return rv.Uint()
	case reflect.String:
		return rv.String()
	case reflect.Slice:
		if rv.IsNil() {
			return nil
		}
		if rv.Len() > 0 {
			vk := visitKey{ptr: rv.Pointer(), typ: rv.Type()}
			if visiting[vk] {
				return "<cycle>"
			}
			visiting[vk] = true
			defer delete(visiting, vk)
		}
		out := make([]any, rv.Len())
		for i := range rv.Len() {
			out[i] = sanitizeCloudReflect(rv.Index(i), visiting)
		}
		return out
	case reflect.Array:
		out := make([]any, rv.Len())
		for i := range rv.Len() {
			out[i] = sanitizeCloudReflect(rv.Index(i), visiting)
		}
		return out
	case reflect.Map:
		if rv.IsNil() {
			return nil
		}
		vk := visitKey{ptr: rv.Pointer(), typ: rv.Type()}
		if visiting[vk] {
			return "<cycle>"
		}
		visiting[vk] = true
		defer delete(visiting, vk)
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			out[formatCloudMapKey(iter.Key())] = sanitizeCloudReflect(iter.Value(), visiting)
		}
		return out
	case reflect.Struct:
		out := make(map[string]any)
		sanitizeCloudStructFields(rv, out, visiting, false)
		return out
	default:
		if rv.CanInterface() {
			return SanitizeText(fmt.Sprint(rv.Interface()))
		}
		return "<" + rv.Type().String() + ">"
	}
}

func formatCloudMapKey(k reflect.Value) string {
	for k.Kind() == reflect.Interface {
		if k.IsNil() {
			return ""
		}
		k = k.Elem()
	}
	switch k.Kind() {
	case reflect.String:
		return k.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(k.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(k.Uint(), 10)
	}
	if k.CanInterface() {
		if tm, ok := k.Interface().(encoding.TextMarshaler); ok {
			if b, err := tryMarshalText(tm); err == nil {
				return string(b)
			}
			if k.Type() == timeType {
				return SanitizeText(k.Interface().(time.Time).Round(0).String())
			}
		}
		if k.Kind() == reflect.Struct || (k.Kind() == reflect.Pointer && !k.IsNil() && k.Elem().Kind() == reflect.Struct) {
			return "<" + k.Type().String() + ">"
		}
		return SanitizeText(fmt.Sprint(k.Interface()))
	}
	return "<" + k.Type().String() + ">"
}

// sanitizeCloudStructFields performs a best-effort field-by-field repair of a
// struct that already failed json.Marshal, honoring exported visibility,
// json:"-", explicit field names, basic omitempty, and anonymous embedding.
func sanitizeCloudStructFields(rv reflect.Value, out map[string]any, visiting map[visitKey]bool, inlined bool) {
	rt := rv.Type()
	for i := range rt.NumField() {
		sf := rt.Field(i)
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		fv := rv.Field(i)
		if sf.Anonymous && name == "" {
			ft := sf.Type
			if ft.Kind() == reflect.Pointer {
				if fv.IsNil() {
					continue
				}
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && ft != timeType && !ft.Implements(jsonMarshalerType) && !ft.Implements(textMarshalerType) {
				if sf.Type.Kind() == reflect.Pointer {
					vk := visitKey{ptr: fv.Pointer(), typ: sf.Type}
					if visiting[vk] {
						continue
					}
					visiting[vk] = true
					sanitizeCloudStructFields(fv.Elem(), out, visiting, true)
					delete(visiting, vk)
				} else {
					sanitizeCloudStructFields(fv, out, visiting, true)
				}
				continue
			}
		}
		if !sf.IsExported() {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		if inlined {
			if _, exists := out[name]; exists {
				continue
			}
		}
		if hasJSONOpt(opts, "omitempty") && isEmptyJSONField(fv) {
			continue
		}
		out[name] = sanitizeCloudReflect(fv, visiting)
	}
}

func hasJSONOpt(opts, target string) bool {
	for opts != "" {
		var opt string
		opt, opts, _ = strings.Cut(opts, ",")
		if opt == target {
			return true
		}
	}
	return false
}

func isEmptyJSONField(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	}
	return false
}

// sanitizeErrorAttr is a slog.HandlerOptions.ReplaceAttr that renders an
// error-valued attribute as its sanitized text.
func sanitizeErrorAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindAny {
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, sanitizedErrorText(err))
		}
	}
	return a
}

// stdoutReachesCloudLogging reports whether this process's stdout is
// likely collected into Cloud Logging: the GCP log format was chosen, or
// the process runs on Cloud Run or in a Kubernetes pod, whose platforms
// collect container output. Every stdout log handler (newStdoutHandler)
// sanitizes error attributes in that case.
func stdoutReachesCloudLogging(useGCP bool) bool {
	return useGCP || cloudrunenv.IsService() || os.Getenv("KUBERNETES_SERVICE_HOST") != ""
}

// logStdout, when set, is where the stdout log handlers write instead of
// os.Stdout. Tests set it.
var logStdout io.Writer

// stdoutWriter returns logStdout if set, else os.Stdout as it is now, so a
// handler built after a caller redirects os.Stdout writes to the redirect.
func stdoutWriter() io.Writer {
	if logStdout != nil {
		return logStdout
	}
	return os.Stdout
}

// newStdoutHandler builds a stdout log handler: the GCP format when useGCP,
// else JSON (with a component attribute when withComponent). Where stdout
// is likely collected into Cloud Logging (stdoutReachesCloudLogging), error
// attributes are rendered as sanitized strings. opts is copied, never
// modified: NewGCPHandler rewrites ReplaceAttr in place, and a caller may
// share opts with another handler.
func newStdoutHandler(w io.Writer, opts *slog.HandlerOptions, component, hubName string, useGCP, withComponent bool) slog.Handler {
	var o slog.HandlerOptions
	if opts != nil {
		o = *opts
	}
	if stdoutReachesCloudLogging(useGCP) {
		prev := o.ReplaceAttr
		o.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if prev != nil {
				a = prev(groups, a)
			}
			return sanitizeErrorAttr(groups, a)
		}
	}
	if useGCP {
		return NewGCPHandler(w, &o, component, hubName)
	}
	h := slog.NewJSONHandler(w, &o)
	if withComponent {
		return h.WithAttrs([]slog.Attr{slog.String(AttrComponent, component)})
	}
	return h
}

// WithSanitizedErrorAttrs wraps h, a handler with no ReplaceAttr hook of
// its own (the OTel log bridge, the agent-container log), so that every
// error-valued attribute it receives, including pre-bound and grouped
// ones, is already a sanitized string.
func WithSanitizedErrorAttrs(h slog.Handler) slog.Handler {
	return &sanitizingHandler{inner: h}
}

// sanitizingHandler is the handler WithSanitizedErrorAttrs returns.
type sanitizingHandler struct {
	inner slog.Handler
}

func (h *sanitizingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *sanitizingHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(sanitizeAttrTree(a))
		return true
	})
	return h.inner.Handle(ctx, nr)
}

func (h *sanitizingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = sanitizeAttrTree(a)
	}
	return &sanitizingHandler{inner: h.inner.WithAttrs(out)}
}

func (h *sanitizingHandler) WithGroup(name string) slog.Handler {
	return &sanitizingHandler{inner: h.inner.WithGroup(name)}
}

// sanitizeAttrTree resolves a and renders every error value in it,
// recursing into groups, as its sanitized text.
func sanitizeAttrTree(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		members := a.Value.Group()
		out := make([]slog.Attr, len(members))
		for i, m := range members {
			out[i] = sanitizeAttrTree(m)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	return sanitizeErrorAttr(nil, a)
}
