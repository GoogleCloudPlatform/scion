package telemetry

import (
	"sync"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	maxRetainedBytes   = 16 << 20
	maxRetainedRecords = 4096
	maxRetainedEntries = 512
)

// admissionBudget counts policy-processed batches while they are retained by
// the pipeline, including synchronous exports in flight and metric retries.
// It bounds encoded payload, not Go heap or SDK internal buffers.
type admissionBudget struct {
	mu                      sync.Mutex
	bytes, records, entries int
}

func (b *admissionBudget) reserve(bytes, records int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if bytes > maxRetainedBytes || records > maxRetainedRecords {
		return status.Error(codes.ResourceExhausted, "telemetry request exceeds retained budget")
	}
	if b.bytes+bytes > maxRetainedBytes || b.records+records > maxRetainedRecords || b.entries+1 > maxRetainedEntries {
		return transientOverload("telemetry retained budget exhausted")
	}
	b.bytes += bytes
	b.records += records
	b.entries++
	return nil
}

func (b *admissionBudget) release(bytes, records, entries int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bytes -= bytes
	b.records -= records
	b.entries -= entries
}

func (b *admissionBudget) resize(oldBytes, oldRecords, newBytes, newRecords int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if newBytes > maxRetainedBytes || newRecords > maxRetainedRecords {
		return status.Error(codes.ResourceExhausted, "telemetry request exceeds retained budget")
	}
	if b.bytes-oldBytes+newBytes > maxRetainedBytes || b.records-oldRecords+newRecords > maxRetainedRecords {
		return transientOverload("telemetry retained budget exhausted")
	}
	b.bytes += newBytes - oldBytes
	b.records += newRecords - oldRecords
	return nil
}

func transientOverload(message string) error {
	st := status.New(codes.ResourceExhausted, message)
	withDetails, err := st.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(time.Second)})
	if err != nil {
		return st.Err()
	}
	return withDetails.Err()
}
