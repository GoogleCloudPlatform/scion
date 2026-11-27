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

package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/trajectory/shipper"
)

func testShipperSegment(t *testing.T) shipper.Segment {
	t.Helper()
	seg, err := shipper.BuildSegment(shipper.SegmentMeta{
		Harness:            "claude",
		NativeRelativePath: ".claude/projects/-workspace/sess-1.jsonl",
		ArtifactKind:       shipper.ArtifactKindJSONLAppend,
		Generation:         0,
		ByteStart:          0,
		SessionID:          "sess-1",
	}, []byte("{\"seq\":1}\n"))
	if err != nil {
		t.Fatalf("BuildSegment: %v", err)
	}
	return seg
}

func TestTrajectoryUploader_HubProxiedFlowAnd412ProceedsToCommit(t *testing.T) {
	seg := testShipperSegment(t)

	for _, putStatus := range []int{http.StatusOK, http.StatusPreconditionFailed} {
		name := "put_200"
		if putStatus == http.StatusPreconditionFailed {
			name = "put_412_proceeds_to_commit"
		}
		t.Run(name, func(t *testing.T) {
			var gotPutToken string
			var gotPutCustomHeader string
			var gotPutBody []byte
			var commitCalled bool

			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == telemetrycontract.TrajectorySegmentsPrepareAgentPath("agent-1"):
					if r.Header.Get("X-Scion-Agent-Token") != "tok-1" {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(telemetrycontract.TrajectorySegmentPrepareResponse{
						SegmentID:  "seg-123",
						State:      telemetrycontract.TrajectorySegmentStateUploadRequired,
						ObjectPath: "hub/obj.seg.gz",
						Upload: &telemetrycontract.TrajectorySegmentUploadInstruction{
							Method: http.MethodPut,
							URL:    srv.URL + telemetrycontract.TrajectorySegmentContentAgentPath("agent-1", "seg-123") + "?exp=9999999999&sig=abc",
							Headers: map[string]string{
								"Content-Type":  "application/gzip",
								"X-Custom-Sign": "verbatim-val",
							},
						},
					})

				case r.Method == http.MethodPut && r.URL.Path == telemetrycontract.TrajectorySegmentContentAgentPath("agent-1", "seg-123"):
					gotPutToken = r.Header.Get("X-Scion-Agent-Token")
					gotPutCustomHeader = r.Header.Get("X-Custom-Sign")
					gotPutBody, _ = io.ReadAll(r.Body)
					w.WriteHeader(putStatus)

				case r.Method == http.MethodPost && r.URL.Path == telemetrycontract.TrajectorySegmentsCommitAgentPath("agent-1"):
					commitCalled = true
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(telemetrycontract.TrajectorySegmentCommitResponse{
						State:        telemetrycontract.TrajectorySegmentStateCommitted,
						SegmentID:    "seg-123",
						ManifestPath: "hub/obj.seg.gz.manifest.json",
					})

				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			client := NewClientWithConfig(srv.URL, "tok-1", "agent-1")
			up := NewTrajectoryUploader(client)
			res, err := up.Ship(context.Background(), seg)
			if err != nil {
				t.Fatalf("Ship: %v", err)
			}
			if res.Outcome != shipper.OutcomeCommitted || res.SegmentID != "seg-123" {
				t.Fatalf("unexpected ShipResult: %+v", res)
			}
			if gotPutToken != "tok-1" {
				t.Fatalf("Hub-proxied PUT missing agent token: got %q", gotPutToken)
			}
			if gotPutCustomHeader != "verbatim-val" {
				t.Fatalf("PUT did not send upload header verbatim: got %q", gotPutCustomHeader)
			}
			if len(gotPutBody) != len(seg.Compressed) {
				t.Fatalf("PUT body len=%d, want %d", len(gotPutBody), len(seg.Compressed))
			}
			if !commitCalled {
				t.Fatalf("expected commit to be called")
			}
		})
	}
}

func TestTrajectoryUploader_ExternalSignedURLDoesNotLeakAgentToken(t *testing.T) {
	seg := testShipperSegment(t)

	var extGotToken string
	var extGotGenMatch string
	extSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		extGotToken = r.Header.Get("X-Scion-Agent-Token")
		extGotGenMatch = r.Header.Get("x-goog-if-generation-match")
		w.WriteHeader(http.StatusPreconditionFailed) // GCS 412 -> proceed to commit
	}))
	defer extSrv.Close()

	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case telemetrycontract.TrajectorySegmentsPrepareAgentPath("agent-1"):
			_ = json.NewEncoder(w).Encode(telemetrycontract.TrajectorySegmentPrepareResponse{
				SegmentID: "seg-gcs",
				State:     telemetrycontract.TrajectorySegmentStateUploadRequired,
				Upload: &telemetrycontract.TrajectorySegmentUploadInstruction{
					Method: http.MethodPut,
					URL:    extSrv.URL + "/bucket/obj.seg.gz?X-Goog-Signature=abc",
					Headers: map[string]string{
						"Content-Type":               "application/gzip",
						"x-goog-if-generation-match": "0",
					},
				},
			})
		case telemetrycontract.TrajectorySegmentsCommitAgentPath("agent-1"):
			_ = json.NewEncoder(w).Encode(telemetrycontract.TrajectorySegmentCommitResponse{
				State:     telemetrycontract.TrajectorySegmentStateCommitted,
				SegmentID: "seg-gcs",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer hubSrv.Close()

	client := NewClientWithConfig(hubSrv.URL, "secret-agent-token", "agent-1")
	res, err := NewTrajectoryUploader(client).Ship(context.Background(), seg)
	if err != nil {
		t.Fatalf("Ship: %v", err)
	}
	if res.Outcome != shipper.OutcomeCommitted {
		t.Fatalf("expected OutcomeCommitted, got %+v", res)
	}
	if extGotToken != "" {
		t.Fatalf("external signed PUT leaked X-Scion-Agent-Token: %q", extGotToken)
	}
	if extGotGenMatch != "0" {
		t.Fatalf("external signed PUT missing x-goog-if-generation-match header: got %q", extGotGenMatch)
	}
}

func TestTrajectoryUploader_IdempotentPrepareCommittedSkipsPutAndCommit(t *testing.T) {
	seg := testShipperSegment(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != telemetrycontract.TrajectorySegmentsPrepareAgentPath("agent-1") {
			t.Fatalf("unexpected second request %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(telemetrycontract.TrajectorySegmentPrepareResponse{
			SegmentID: "seg-already",
			State:     telemetrycontract.TrajectorySegmentStateCommitted,
		})
	}))
	defer srv.Close()

	res, err := NewTrajectoryUploader(NewClientWithConfig(srv.URL, "tok", "agent-1")).Ship(context.Background(), seg)
	if err != nil {
		t.Fatalf("Ship: %v", err)
	}
	if res.Outcome != shipper.OutcomeCommitted || res.SegmentID != "seg-already" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestTrajectoryUploader_OutcomeMappingTable(t *testing.T) {
	seg := testShipperSegment(t)
	tests := []struct {
		name        string
		status      int
		code        string
		wantOutcome shipper.ShipOutcome
	}{
		{"404_experiment_off", http.StatusNotFound, "not_found", shipper.OutcomeCaptureUnavailable},
		{"409_conflict", http.StatusConflict, telemetrycontract.TrajectoryErrSegmentConflict, shipper.OutcomeConflict},
		{"400_invalid", http.StatusBadRequest, telemetrycontract.TrajectoryErrInvalidSegment, shipper.OutcomeRejected},
		{"413_too_large", http.StatusRequestEntityTooLarge, telemetrycontract.TrajectoryErrSegmentTooLarge, shipper.OutcomeRejected},
		{"413_run_cap", http.StatusRequestEntityTooLarge, telemetrycontract.TrajectoryErrRunCapExceeded, shipper.OutcomeRunCapExceeded},
		{"403_capture_disabled", http.StatusForbidden, telemetrycontract.TrajectoryErrCaptureDisabled, shipper.OutcomePolicyRefused},
		{"403_capture_metadata_only", http.StatusForbidden, telemetrycontract.TrajectoryErrCaptureMetadataOnly, shipper.OutcomePolicyRefused},
		{"403_wrong_agent", http.StatusForbidden, "forbidden", shipper.OutcomeRetryable},
		{"410_expired", http.StatusGone, telemetrycontract.TrajectoryErrSegmentExpired, shipper.OutcomeRetryable},
		{"422_verification_failed", http.StatusUnprocessableEntity, telemetrycontract.TrajectoryErrVerificationFailed, shipper.OutcomeRetryable},
		{"503_unavailable", http.StatusServiceUnavailable, "unavailable", shipper.OutcomeRetryable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{
						"code":    tc.code,
						"message": "test error",
					},
				})
			}))
			defer srv.Close()

			res, _ := NewTrajectoryUploader(NewClientWithConfig(srv.URL, "tok", "agent-1")).Ship(context.Background(), seg)
			if res.Outcome != tc.wantOutcome {
				t.Fatalf("status=%d code=%s: got outcome %q, want %q", tc.status, tc.code, res.Outcome, tc.wantOutcome)
			}
		})
	}
}

func TestTrajectoryUploader_PutErrorsAlwaysRetryable(t *testing.T) {
	statuses := []struct {
		name   string
		status int
		code   string
	}{
		{"400", http.StatusBadRequest, telemetrycontract.TrajectoryErrInvalidSegment},
		{"404", http.StatusNotFound, "not_found"},
		{"409", http.StatusConflict, telemetrycontract.TrajectoryErrSegmentConflict},
		{"413", http.StatusRequestEntityTooLarge, telemetrycontract.TrajectoryErrSegmentTooLarge},
		{"500", http.StatusInternalServerError, "internal"},
		{"503", http.StatusServiceUnavailable, "unavailable"},
	}

	for _, mode := range []string{"external_signed", "hub_proxied"} {
		for _, tc := range statuses {
			t.Run(mode+"_"+tc.name, func(t *testing.T) {
				var extSrv *httptest.Server
				if mode == "external_signed" {
					extSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(tc.status)
						_, _ = w.Write([]byte("storage error"))
					}))
					defer extSrv.Close()
				}

				var hubSrv *httptest.Server
				hubSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case telemetrycontract.TrajectorySegmentsPrepareAgentPath("agent-1"):
						uploadURL := hubSrv.URL + telemetrycontract.TrajectorySegmentContentAgentPath("agent-1", "seg-put") + "?exp=9999999999&sig=abc"
						if extSrv != nil {
							uploadURL = extSrv.URL + "/bucket/obj.seg.gz?X-Goog-Signature=abc"
						}
						_ = json.NewEncoder(w).Encode(telemetrycontract.TrajectorySegmentPrepareResponse{
							SegmentID: "seg-put",
							State:     telemetrycontract.TrajectorySegmentStateUploadRequired,
							Upload: &telemetrycontract.TrajectorySegmentUploadInstruction{
								Method: http.MethodPut,
								URL:    uploadURL,
								Headers: map[string]string{
									"Content-Type": "application/gzip",
								},
							},
						})
					case telemetrycontract.TrajectorySegmentContentAgentPath("agent-1", "seg-put"):
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(tc.status)
						_ = json.NewEncoder(w).Encode(map[string]any{
							"error": map[string]string{"code": tc.code, "message": "put failed"},
						})
					default:
						t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer hubSrv.Close()

				home := t.TempDir()
				rel := ".claude/projects/-workspace/sess-puterr.jsonl"
				artPath := filepath.Join(home, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(artPath), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(artPath, nil, 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}

				up := NewTrajectoryUploader(NewClientWithConfig(hubSrv.URL, "tok", "agent-1"))
				s, err := shipper.New(shipper.Config{HomeDir: home, RunID: "run-1", Uploader: up})
				if err != nil {
					t.Fatalf("shipper.New: %v", err)
				}
				if _, err := s.Baseline(context.Background()); err != nil {
					t.Fatalf("Baseline: %v", err)
				}
				if err := os.WriteFile(artPath, []byte("{\"seq\":1}\n"), 0o644); err != nil {
					t.Fatalf("append: %v", err)
				}

				roundRes, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
				if err != nil {
					t.Fatalf("RunRound: %v", err)
				}
				if roundRes.SegmentsShipped != 0 {
					t.Fatalf("expected 0 segments shipped, got %d", roundRes.SegmentsShipped)
				}
				if roundRes.GenerationBumps != 0 {
					t.Fatalf("PUT %s must not bump generation, got %d", tc.name, roundRes.GenerationBumps)
				}
				if roundRes.CaptureUnavailable || roundRes.PolicyRefused {
					t.Fatalf("PUT %s must not trigger backoff/policy refusal: %+v", tc.name, roundRes)
				}
				st, ok, err := shipper.LoadState(home)
				if err != nil || !ok {
					t.Fatalf("LoadState: ok=%v err=%v", ok, err)
				}
				entry := st.Artifacts[rel]
				if entry.Rejected {
					t.Fatalf("PUT %s must not park artifact as Rejected", tc.name)
				}
				if entry.Pending == nil {
					t.Fatalf("PUT %s must preserve pending segment for retry", tc.name)
				}
			})
		}
	}
}

func TestTrajectoryUploader_ProxiedPutUsesUploadTimeoutNotDefaultClientTimeout(t *testing.T) {
	seg := testShipperSegment(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == telemetrycontract.TrajectorySegmentContentAgentPath("agent-1", "seg-slow") {
			// Exceed the client's 50ms base Timeout to prove proxied PUTs use
			// trajectoryUploadHTTPTimeout (2m) instead of client.Timeout.
			time.Sleep(150 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClientWithConfig(srv.URL, "tok-1", "agent-1")
	client.client.Timeout = 50 * time.Millisecond

	inst := telemetrycontract.TrajectorySegmentUploadInstruction{
		Method: http.MethodPut,
		URL:    srv.URL + telemetrycontract.TrajectorySegmentContentAgentPath("agent-1", "seg-slow") + "?exp=9999999999&sig=abc",
		Headers: map[string]string{
			"Content-Type": "application/gzip",
		},
	}
	status, err := client.UploadTrajectorySegment(context.Background(), inst, seg.Compressed)
	if err != nil {
		t.Fatalf("UploadTrajectorySegment failed against slow proxied PUT (likely used short client.Timeout instead of trajectoryUploadHTTPTimeout): %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200", status)
	}
}
