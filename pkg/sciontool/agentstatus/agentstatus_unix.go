/*
Copyright 2025 The Scion Authors.
*/

//go:build unix

package agentstatus

import (
	"context"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// Run performs the status update: the local activity (handlers keep
// waiting_for_input sticky through the next tool-end hook), the event log
// line, and the Hub report when a Hub is configured. Failures are logged,
// never returned: a status update never fails its caller.
func Run(definition Definition, message string) {
	if message == "" {
		message = definition.DefaultMessage
	}
	statusHandler := handlers.NewStatusHandler()
	loggingHandler := handlers.NewLoggingHandler()

	if err := statusHandler.UpdateActivity(definition.Activity, ""); err != nil {
		log.Error("Failed to update status: %v", err)
	}

	logMessage := fmt.Sprintf("%s: %s", definition.eventPrefix, message)
	if err := loggingHandler.LogEvent(string(definition.Activity), logMessage); err != nil {
		log.Error("Failed to log event: %v", err)
	}

	if hubClient := hub.NewClient(); hubClient != nil && hubClient.IsConfigured() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := hubClient.UpdateStatus(ctx, definition.HubUpdate(message)); err != nil {
			log.Error("Failed to report to Hub: %v", err)
		}
	}

	log.Info("%s: %s", definition.outputPrefix, message)
}
