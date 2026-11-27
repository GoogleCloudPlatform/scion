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

// Command conduit-c19 runs the Criterion 19 Conduit load profile against a
// live hub or load balancer using the shared internal/conduittest fakes and
// hack/conduit-c19 driver library. It is validation tooling and is not shipped.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"

	c19driver "github.com/GoogleCloudPlatform/scion/hack/conduit-c19"
	"github.com/GoogleCloudPlatform/scion/internal/conduittest"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
)

func main() {
	var (
		hubURL          = flag.String("hub-url", "", "Hub or load-balancer base URL (e.g. https://hub.example.com)")
		brokerID        = flag.String("broker-id", "", "Runtime broker ID for HMAC-authenticated deliver calls")
		brokerSecretB64 = flag.String("broker-secret-b64", "", "Base64-encoded broker HMAC secret")
		agentsJSONPath  = flag.String("agents-json", "", "Path to JSON array of AgentTarget objects ({agent_id, project_id, launch_id, token, hub_url})")
		duration        = flag.Duration("duration", 30*time.Second, "Active load duration")
		deliverInterval = flag.Duration("deliver-interval", 2*time.Second, "Per-agent delivery interval")
		largeBodies     = flag.Int("large-bodies", 4, "Number of 512 KiB deliver-message calls")
		jsonOut         = flag.Bool("json", false, "Emit summary as JSON")
	)
	flag.Parse()

	if *hubURL == "" || *brokerID == "" || *brokerSecretB64 == "" || *agentsJSONPath == "" {
		log.Fatal("required flags: -hub-url, -broker-id, -broker-secret-b64, -agents-json")
	}

	secret, err := base64.StdEncoding.DecodeString(*brokerSecretB64)
	if err != nil {
		log.Fatalf("decode -broker-secret-b64: %v", err)
	}
	raw, err := os.ReadFile(*agentsJSONPath)
	if err != nil {
		log.Fatalf("read -agents-json: %v", err)
	}
	var targets []c19driver.AgentTarget
	if err := json.Unmarshal(raw, &targets); err != nil {
		log.Fatalf("parse -agents-json: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	broker, err := conduittest.StartBroker(conduittest.BrokerOptions{
		HubURL:       *hubURL,
		BrokerID:     *brokerID,
		BrokerSecret: secret,
	})
	if err != nil {
		log.Fatalf("start fake broker: %v", err)
	}
	defer func() { _ = broker.Close() }()

	waitCtx, waitCancel := context.WithTimeout(ctx, 15*time.Second)
	err = broker.WaitHealthy(waitCtx)
	waitCancel()
	if err != nil {
		log.Fatalf("wait for broker healthy: %v", err)
	}

	res, err := c19driver.Run(ctx, c19driver.Config{
		Agents:          targets,
		DefaultHubURL:   *hubURL,
		Duration:        *duration,
		DeliverInterval: *deliverInterval,
		LargeBodyCount:  *largeBodies,
		Deliver: func(callCtx context.Context, agentID, body string) (int, string, error) {
			text := body
			status, resp, err := broker.DeliverMessage(callCtx, agentID, &agentcontrol.DeliverRequest{
				V:          1,
				DeliveryID: "c19-live-" + uuid.NewString(),
				Op:         agentcontrol.OpPaste,
				Text:       &text,
				DeadlineMS: 92000,
				SubmitKeys: []string{"Enter"},
			})
			return status, string(resp.Outcome), err
		},
	})
	if err != nil {
		log.Fatalf("c19 run failed: %v", err)
	}

	stats := c19driver.SummarizeLatencies(res.DeliverLatencies)
	if *jsonOut {
		out := map[string]any{
			"agents":               res.AgentCount,
			"deliver_attempts":     res.DeliverAttempts,
			"deliver_succeeded":    res.DeliverSucceeded,
			"deliver_429s":         res.Deliver429s,
			"deliver_errors":       res.DeliverErrors,
			"large_body_succeeded": res.LargeBodySucceeded,
			"large_body_errors":    res.LargeBodyErrors,
			"deliver_latency":      stats,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	} else {
		fmt.Printf("conduit-c19 complete: agents=%d deliveries=%d/%d 429s=%d errors=%d large=%d p50=%s p99=%s max=%s\n",
			res.AgentCount, res.DeliverSucceeded, res.DeliverAttempts, res.Deliver429s,
			res.DeliverErrors, res.LargeBodySucceeded, stats.P50, stats.P99, stats.Max)
	}
	if res.DeliverErrors > 0 || res.Deliver429s > 0 {
		os.Exit(1)
	}
}
