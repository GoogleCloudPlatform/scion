package telemetry

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/stats"
)

type grpcPermitKey struct{}

// grpcPermit owns one pre-decode slot until gRPC reports terminal RPC work.
// Context cancellation releases only streams that never reached dispatch.
type grpcPermit struct {
	mu                sync.Mutex
	slots             chan struct{}
	cancel            context.CancelFunc
	started, released bool
}

func (p *grpcPermit) markStarted() {
	p.mu.Lock()
	if p.released {
		// Cancellation may have run after the tap but before the server
		// worker reached InHeader. InHeader runs before recv/unmarshal, so
		// reacquire here before allowing that work to begin. This worker may
		// wait; the tap on the connection I/O goroutine never does.
		p.mu.Unlock()
		p.slots <- struct{}{}
		p.mu.Lock()
		p.released = false
	}
	p.started = true
	p.mu.Unlock()
}

func (p *grpcPermit) cancelBeforeWork() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started {
		p.releaseLocked()
	}
}

func (p *grpcPermit) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.releaseLocked()
}

func (p *grpcPermit) releaseLocked() {
	if p.released {
		return
	}
	p.released = true
	<-p.slots
	p.cancel()
}

func newGRPCPermitContext(ctx context.Context, slots chan struct{}) context.Context {
	bounded, cancel := context.WithTimeout(ctx, intakeDeadline)
	permit := &grpcPermit{slots: slots, cancel: cancel}
	context.AfterFunc(bounded, permit.cancelBeforeWork)
	return context.WithValue(bounded, grpcPermitKey{}, permit)
}

// grpcAdmissionStats receives End after decoding and the unary handler have
// both finished, including malformed and oversized requests.
type grpcAdmissionStats struct{}

func (grpcAdmissionStats) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}
func (grpcAdmissionStats) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}
func (grpcAdmissionStats) HandleConn(context.Context, stats.ConnStats) {}
func (grpcAdmissionStats) HandleRPC(ctx context.Context, event stats.RPCStats) {
	permit, ok := ctx.Value(grpcPermitKey{}).(*grpcPermit)
	if !ok {
		return
	}
	switch event.(type) {
	case *stats.InHeader:
		permit.markStarted()
	case *stats.End:
		permit.finish()
	}
}

const maxGRPCConnections = 64
const grpcHandshakeTimeout = 5 * time.Second
