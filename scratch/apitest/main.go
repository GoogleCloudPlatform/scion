// Command apitest drives API-level multi-hub integration/stress traffic against
// two running Scion hubs that share one CloudSQL Postgres instance. It validates
// the connection-pool / keepalive fixes and multi-replica behavior through the
// real HTTP API. Run it ON a hub VM so it reaches both hubs over the fast
// internal network. Not part of the product.
//
// Env:
//   A_BASE, B_BASE   base URLs (e.g. http://localhost:8080, http://10.128.15.241:8080)
//   A_TOK, B_TOK     admin bearer tokens (per-hub signing keys)
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type hub struct {
	name string
	base string
	tok  string
}

var client = &http.Client{Timeout: 35 * time.Second}

func req(h hub, method, path string, body any) (int, []byte, time.Duration) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	r, _ := http.NewRequest(method, h.base+path, rdr)
	r.Header.Set("Authorization", "Bearer "+h.tok)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := client.Do(r)
	d := time.Since(start)
	if err != nil {
		return 0, []byte(err.Error()), d
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, rb, d
}

func pct(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	i := int(float64(len(ds)) * p)
	if i >= len(ds) {
		i = len(ds) - 1
	}
	return ds[i]
}

func main() {
	A := hub{"A", os.Getenv("A_BASE"), os.Getenv("A_TOK")}
	B := hub{"B", os.Getenv("B_BASE"), os.Getenv("B_TOK")}
	hubs := []hub{A, B}

	// ---- Phase 1: concurrent CRUD storm across both hubs ----
	fmt.Println("== Phase 1: concurrent project CRUD storm (both hubs) ==")
	const workers, iters = 24, 30
	var ok, fail, stalls int64
	latMu := sync.Mutex{}
	lat := map[string][]time.Duration{"A": {}, "B": {}}
	var wg sync.WaitGroup
	t0 := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			h := hubs[w%2]
			for i := 0; i < iters; i++ {
				name := fmt.Sprintf("stress-%d-%d-%s", w, i, uuid.NewString()[:8])
				st, body, d := req(h, "POST", "/api/v1/projects", map[string]string{"name": name})
				if d > 2*time.Second {
					atomic.AddInt64(&stalls, 1)
				}
				if st != 201 && st != 200 {
					atomic.AddInt64(&fail, 1)
					if i == 0 {
						fmt.Printf("  [%s] create failed st=%d body=%.120s\n", h.name, st, body)
					}
					continue
				}
				var pr struct {
					ID string `json:"id"`
				}
				json.Unmarshal(body, &pr)
				req(h, "GET", "/api/v1/projects/"+pr.ID, nil)
				req(h, "GET", "/api/v1/projects?limit=5", nil)
				dst, _, dd := req(h, "DELETE", "/api/v1/projects/"+pr.ID, nil)
				if dd > 2*time.Second {
					atomic.AddInt64(&stalls, 1)
				}
				if dst >= 200 && dst < 300 {
					atomic.AddInt64(&ok, 1)
				} else {
					atomic.AddInt64(&fail, 1)
				}
				latMu.Lock()
				lat[h.name] = append(lat[h.name], d)
				latMu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	dur := time.Since(t0)
	total := int64(workers * iters)
	fmt.Printf("  full CRUD cycles ok=%d fail=%d of %d in %s (%.0f cycles/s), stalls(>2s)=%d\n",
		ok, fail, total, dur.Truncate(time.Millisecond), float64(total)/dur.Seconds(), stalls)
	for _, n := range []string{"A", "B"} {
		fmt.Printf("  hub %s create-latency p50=%s p95=%s max=%s (n=%d)\n",
			n, pct(lat[n], 0.5), pct(lat[n], 0.95), pct(lat[n], 1.0), len(lat[n]))
	}

	// ---- Phase 2: cross-replica read-after-write (create A, read B) ----
	fmt.Println("== Phase 2: cross-replica read-after-write (create on A, GET on B) ==")
	const rw = 40
	var immediate, delayed, miss int
	for i := 0; i < rw; i++ {
		name := "raw-" + uuid.NewString()[:10]
		st, body, _ := req(A, "POST", "/api/v1/projects", map[string]string{"name": name})
		if st != 201 && st != 200 {
			miss++
			continue
		}
		var pr struct {
			ID string `json:"id"`
		}
		json.Unmarshal(body, &pr)
		got := false
		for attempt := 0; attempt < 10; attempt++ {
			s2, _, _ := req(B, "GET", "/api/v1/projects/"+pr.ID, nil)
			if s2 == 200 {
				if attempt == 0 {
					immediate++
				} else {
					delayed++
				}
				got = true
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !got {
			miss++
		}
		req(A, "DELETE", "/api/v1/projects/"+pr.ID, nil)
	}
	fmt.Printf("  read-after-write: immediate=%d delayed=%d miss=%d of %d\n", immediate, delayed, miss, rw)

	// ---- Phase 3: conflict -> HTTP 409 (concurrent duplicate-ID creates) ----
	fmt.Println("== Phase 3: concurrent duplicate-ID create -> expect exactly one 201, rest 409 ==")
	const rounds = 25
	var created, conflict, other int
	for i := 0; i < rounds; i++ {
		id := uuid.NewString()
		name := "dup-" + id[:8]
		var c201, c409, cother int64
		var w2 sync.WaitGroup
		// 4 concurrent creators (2 per hub) racing on the same explicit ID.
		for k := 0; k < 4; k++ {
			w2.Add(1)
			go func(k int) {
				defer w2.Done()
				h := hubs[k%2]
				st, _, _ := req(h, "POST", "/api/v1/projects", map[string]any{"id": id, "name": name})
				switch {
				case st == 201 || st == 200:
					atomic.AddInt64(&c201, 1)
				case st == 409:
					atomic.AddInt64(&c409, 1)
				default:
					atomic.AddInt64(&cother, 1)
				}
			}(k)
		}
		w2.Wait()
		created += int(c201)
		conflict += int(c409)
		other += int(cother)
		req(A, "DELETE", "/api/v1/projects/"+id, nil)
	}
	fmt.Printf("  over %d rounds (4 racers each): 201=%d 409=%d other=%d (ideal: 201==%d, 409==%d)\n",
		rounds, created, conflict, other, rounds, rounds*3)

	// ---- Phase 4: idle-then-burst (the stale-connection scenario) ----
	idleStr := os.Getenv("IDLE_SECONDS")
	idle := 75
	fmt.Sscanf(idleStr, "%d", &idle)
	fmt.Printf("== Phase 4: idle %ds then burst (validates keepalive/idle-recycle fix) ==\n", idle)
	for _, h := range hubs { // warm the pools
		for i := 0; i < 5; i++ {
			req(h, "GET", "/api/v1/projects?limit=1", nil)
		}
	}
	fmt.Printf("  pools warm; sleeping %ds to force idle...\n", idle)
	time.Sleep(time.Duration(idle) * time.Second)
	for _, h := range hubs {
		var first time.Duration
		var maxd time.Duration
		for i := 0; i < 10; i++ {
			st, _, d := req(h, "GET", "/api/v1/projects?limit=1", nil)
			if i == 0 {
				first = d
			}
			if d > maxd {
				maxd = d
			}
			if st != 200 {
				fmt.Printf("  [%s] burst req %d unexpected st=%d\n", h.name, i, st)
			}
		}
		verdict := "OK"
		if first > 2*time.Second {
			verdict = "STALL (likely dead idle conn)"
		}
		fmt.Printf("  hub %s post-idle first-request=%s max=%s -> %s\n",
			h.name, first.Truncate(time.Millisecond), maxd.Truncate(time.Millisecond), verdict)
	}
	fmt.Println("== done ==")
}
