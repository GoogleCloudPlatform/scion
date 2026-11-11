/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestReceiverBoundPorts(t *testing.T) {
	t.Run("before Start returns configured ports", func(t *testing.T) {
		r := NewReceiver(&Config{GRPCPort: 14317, HTTPPort: 14318}, nil)
		if g, h := r.BoundPorts(); g != 14317 || h != 14318 {
			t.Fatalf("BoundPorts() = %d/%d, want 14317/14318", g, h)
		}
	})

	t.Run("ephemeral ports are reported and dialable", func(t *testing.T) {
		r := NewReceiver(&Config{}, nil)
		if err := r.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Stop(context.Background()) }()
		g, h := r.BoundPorts()
		if g == 0 || h == 0 || g == h {
			t.Fatalf("BoundPorts() = %d/%d, want two distinct non-zero ports", g, h)
		}
		for _, port := range []int{g, h} {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
			if err != nil {
				t.Fatalf("dial bound port %d: %v", port, err)
			}
			_ = conn.Close()
		}
	})

	t.Run("failed HTTP bind falls back to configured ports", func(t *testing.T) {
		busy, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = busy.Close() }()
		busyPort := busy.Addr().(*net.TCPAddr).Port
		r := NewReceiver(&Config{GRPCPort: 0, HTTPPort: busyPort}, nil)
		if err := r.Start(context.Background()); err == nil {
			_ = r.Stop(context.Background())
			t.Fatal("Start succeeded on a busy HTTP port")
		}
		if g, h := r.BoundPorts(); g != 0 || h != busyPort {
			t.Fatalf("BoundPorts() after failed Start = %d/%d, want configured 0/%d", g, h, busyPort)
		}
	})
}
