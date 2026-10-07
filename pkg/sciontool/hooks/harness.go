/*
Copyright 2025 The Scion Authors.
*/

package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// HarnessProcessor processes hook events from agent harnesses.
// It normalizes events from different harness dialects (Claude, Gemini, etc.)
// and dispatches them to registered handlers.
type HarnessProcessor struct {
	// Dialects maps dialect names to their parsers
	Dialects map[string]Dialect

	// DefaultDialect is used when no dialect is specified
	DefaultDialect string

	// Handlers are called for each processed event
	Handlers []Handler
}

// NewHarnessProcessor creates a new harness processor with default dialects.
func NewHarnessProcessor() *HarnessProcessor {
	return &HarnessProcessor{
		Dialects:       make(map[string]Dialect),
		DefaultDialect: "claude",
		Handlers:       nil,
	}
}

// RegisterDialect adds a dialect parser.
func (p *HarnessProcessor) RegisterDialect(dialect Dialect) {
	p.Dialects[dialect.Name()] = dialect
}

// AddHandler registers a handler for processed events.
func (p *HarnessProcessor) AddHandler(handler Handler) {
	p.Handlers = append(p.Handlers, handler)
}

// ProcessFromStdin reads JSON data from stdin and processes it.
func (p *HarnessProcessor) ProcessFromStdin(dialectName string) error {
	// Check if stdin has data
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		// Stdin is a terminal, no data
		return nil
	}

	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}

	if len(data) == 0 {
		return nil
	}

	return p.ProcessJSON(data, dialectName)
}

// ProcessJSON parses JSON data and processes the event.
func (p *HarnessProcessor) ProcessJSON(data []byte, dialectName string) error {
	var rawData map[string]interface{}
	if err := json.Unmarshal(data, &rawData); err != nil {
		return fmt.Errorf("parsing JSON: %w", err)
	}

	return p.ProcessRaw(rawData, dialectName)
}

// ProcessRaw processes a raw event map.
func (p *HarnessProcessor) ProcessRaw(data map[string]interface{}, dialectName string) error {
	event, err := p.ParseRaw(data, dialectName)
	if err != nil {
		return err
	}
	return p.Dispatch(event)
}

// ParseRaw normalizes a raw event map with the named dialect (or the default
// dialect when dialectName is empty) without dispatching it.
func (p *HarnessProcessor) ParseRaw(data map[string]interface{}, dialectName string) (*Event, error) {
	if dialectName == "" {
		dialectName = p.DefaultDialect
	}

	dialect, ok := p.Dialects[dialectName]
	if !ok {
		return nil, fmt.Errorf("unknown dialect: %s", dialectName)
	}

	event, err := dialect.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parsing event with dialect %s: %w", dialectName, err)
	}
	return event, nil
}

// Dispatch calls every registered handler for an already parsed event.
func (p *HarnessProcessor) Dispatch(event *Event) error {
	return p.dispatchEvent(event)
}

// dispatchEvent calls all registered handlers for an event.
func (p *HarnessProcessor) dispatchEvent(event *Event) error {
	var errs []error

	for _, handler := range p.Handlers {
		if err := handler(event); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("handler errors: %v", errs)
	}
	return nil
}
