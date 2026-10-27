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

package txdiag

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// stackDepth is how many caller PCs are recorded per transaction or
// connection checkout. The deepest known path (a non-transactional ent
// mutation that opens sqlgraph's internal transaction) has about 22
// infrastructure frames ahead of the first caller frame: database/sql,
// entsql, this package, sqlgraph, and the generated ent code with withHooks
// and the two entc mutation hooks. 32 leaves headroom; overflowing it would
// silently report op=unknown.
const stackDepth = 32

// siteFrames is how many non-infrastructure frames the WARN's site lists.
const siteFrames = 3

// maxOps caps the number of distinct op values. Past it, an op is reported
// as OpOther, which bounds both the metric label and the rate limiter.
const maxOps = 256

// OpOther is the op reported once maxOps distinct ops have been seen.
const OpOther = "other"

// opUnknown is the op when no non-infrastructure frame was found.
const opUnknown = "unknown"

const modulePrefix = "github.com/GoogleCloudPlatform/scion/"

// infraPrefixes are function-name prefixes of frames that never name an op:
// database/sql, the runtime, ent itself, the generated ent code and entc
// (including the entc mutation hooks every ent mutation passes through), and
// this package.
var infraPrefixes = []string{
	"database/sql.",
	"runtime.",
	"entgo.io/ent.",
	"entgo.io/ent/",
	modulePrefix + "pkg/ent.",
	modulePrefix + "pkg/ent/",
	modulePrefix + "pkg/observability/txdiag.",
}

// funnelPkg and funnelFuncs name the generic transaction and begin helpers
// in pkg/store/entadapter. A frame of one of them (or of a closure inside
// it) is skipped, so the op names the helper's caller.
const funnelPkg = modulePrefix + "pkg/store/entadapter."

var funnelFuncs = []string{
	"runInEntTx",
	"(*CompositeStore).WithTx",
	"runSerializableEntTx",
	"(*CompositeStore).RunSerializable",
	"(*ConduitRegistryStore).begin",
	"(*AgentStore).beginLaunchTx",
}

// isInfra reports whether fn (a full function name from runtime.Frame) is an
// infrastructure frame.
func isInfra(fn string) bool {
	for _, p := range infraPrefixes {
		if strings.HasPrefix(fn, p) {
			return true
		}
	}
	if rest, ok := strings.CutPrefix(fn, funnelPkg); ok {
		for _, f := range funnelFuncs {
			if matchFunc(rest, f) {
				return true
			}
		}
	}
	return false
}

// matchFunc reports whether name is f itself, an instantiation of generic f
// ("f[...]"), or a closure inside f ("f.func1", "f.gowrap1").
func matchFunc(name, f string) bool {
	rest, ok := strings.CutPrefix(name, f)
	if !ok {
		return false
	}
	return rest == "" || strings.HasPrefix(rest, "[") || strings.HasPrefix(rest, ".")
}

// shortFunc strips the package path, keeping the last path element:
// "github.com/x/y/pkg/hub.(*Server).f" becomes "hub.(*Server).f".
func shortFunc(fn string) string {
	if i := strings.LastIndex(fn, "/"); i >= 0 {
		return fn[i+1:]
	}
	return fn
}

type frameInfo struct {
	fn    string // short function name
	file  string // base file name
	line  int
	infra bool
}

// callsites caches the symbolized frames of each PC and interns op names.
// The PC cache is bounded by the size of the program's code; the op set is
// capped at maxOps.
type callsites struct {
	mu   sync.RWMutex
	pcs  map[uintptr][]frameInfo
	ops  map[string]string
	full bool
}

func newCallsites() *callsites {
	return &callsites{pcs: make(map[uintptr][]frameInfo), ops: make(map[string]string)}
}

// frames returns the frames of pc, expanding inlined calls. Symbolization
// runs once per PC.
func (c *callsites) frames(pc uintptr) []frameInfo {
	c.mu.RLock()
	f, ok := c.pcs[pc]
	c.mu.RUnlock()
	if ok {
		return f
	}
	it := runtime.CallersFrames([]uintptr{pc})
	for {
		fr, more := it.Next()
		if fr.Function != "" {
			f = append(f, frameInfo{
				fn:    shortFunc(fr.Function),
				file:  filepath.Base(fr.File),
				line:  fr.Line,
				infra: isInfra(fr.Function),
			})
		}
		if !more {
			break
		}
	}
	c.mu.Lock()
	c.pcs[pc] = f
	c.mu.Unlock()
	return f
}

// op returns the capped op for a recorded stack: the first
// non-infrastructure frame's short function name, or OpOther past the cap.
func (c *callsites) op(pcs []uintptr) string {
	// Fast path: walk the cached frames under one read lock.
	c.mu.RLock()
	name, miss := "", false
walk:
	for _, pc := range pcs {
		f, ok := c.pcs[pc]
		if !ok {
			miss = true
			break
		}
		for _, fr := range f {
			if !fr.infra {
				name = fr.fn
				break walk
			}
		}
	}
	if name != "" {
		if v, ok := c.ops[name]; ok {
			c.mu.RUnlock()
			return v
		}
		if c.full {
			// Past the cap the op can never be added: no exclusive lock.
			c.mu.RUnlock()
			return OpOther
		}
	}
	c.mu.RUnlock()
	if name != "" && !miss {
		op, _ := c.intern(name)
		return op
	}
	// Slow path: symbolize the PCs not yet cached.
	for _, pc := range pcs {
		for _, f := range c.frames(pc) {
			if !f.infra {
				op, _ := c.intern(f.fn)
				return op
			}
		}
	}
	op, _ := c.intern(opUnknown)
	return op
}

// intern returns name if it is (or can become) one of the first maxOps ops,
// and OpOther otherwise. capped is true exactly once, when the cap is first
// exceeded.
func (c *callsites) intern(name string) (op string, capped bool) {
	c.mu.RLock()
	v, ok := c.ops[name]
	c.mu.RUnlock()
	if ok {
		return v, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.ops[name]; ok {
		return v, false
	}
	if len(c.ops) >= maxOps {
		first := !c.full
		c.full = true
		return OpOther, first
	}
	c.ops[name] = name
	return name, false
}

// site formats the first siteFrames non-infrastructure frames as
// "fn file:line <- fn <- fn". It is called only when a WARN is emitted.
func (c *callsites) site(pcs []uintptr) string {
	var b strings.Builder
	n := 0
	for _, pc := range pcs {
		for _, f := range c.frames(pc) {
			if f.infra {
				continue
			}
			switch n {
			case 0:
				fmt.Fprintf(&b, "%s %s:%d", f.fn, f.file, f.line)
			default:
				b.WriteString(" <- ")
				b.WriteString(f.fn)
			}
			n++
			if n == siteFrames {
				return b.String()
			}
		}
	}
	if n == 0 {
		return opUnknown
	}
	return b.String()
}
