//go:build !unix

/*
Copyright 2025 The Scion Authors.
*/

package agentstatus

import (
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// Run refuses the status update: agents run only on unix, and the local
// agent state it writes needs unix file locking. It fails closed and logs
// the refusal; a status update never fails its caller, so nothing is
// returned.
func Run(definition Definition, message string) {
	log.Error("%v", fmt.Errorf("agent status %s: %w on this platform", definition.Activity, errors.ErrUnsupported))
}
