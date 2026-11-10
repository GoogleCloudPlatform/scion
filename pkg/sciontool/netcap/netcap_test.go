/*
Copyright 2026 The Scion Authors.
*/

package netcap

import (
	"errors"
	"testing"
)

func TestParseStatus(t *testing.T) {
	s, err := parseStatus("CapInh:\t0\nCapPrm:\t1000\nCapEff:\t0\nCapBnd:\tffff\n")
	if err != nil {
		t.Fatal(err)
	}
	if s.prm != 0x1000 || s.bnd != 0xffff || s.amb != 0 {
		t.Errorf("parsed %+v", s)
	}
	if !s.holds(capNetAdmin) {
		t.Error("holds(12) = false")
	}
	if _, err := parseStatus("CapInh:\t0\nCapPrm:\t0\nCapEff:\t0\n"); err == nil {
		t.Error("missing CapBnd accepted")
	}
	if _, err := parseStatus("CapInh:\tzz\nCapPrm:\t0\nCapEff:\t0\nCapBnd:\t0\n"); err == nil {
		t.Error("bad hex accepted")
	}
}

func TestVerifyNone(t *testing.T) {
	clear := "CapInh:\t0\nCapPrm:\t0\nCapEff:\t0\nCapBnd:\t0\n"
	held := "CapInh:\t0\nCapPrm:\t0\nCapEff:\t0\nCapBnd:\t1000\n"
	if err := verifyNone([]string{clear, clear}, capNetAdmin); err != nil {
		t.Errorf("all clear: %v", err)
	}
	if err := verifyNone([]string{clear, held, clear}, capNetAdmin); !errors.Is(err, ErrStillHeld) {
		t.Errorf("one thread held: %v", err)
	}
	if err := verifyNone(nil, capNetAdmin); err == nil {
		t.Error("no threads accepted")
	}
}

func TestAllUsable(t *testing.T) {
	usable := "CapInh:\t0\nCapPrm:\t1000\nCapEff:\t1000\nCapBnd:\t1000\n"
	notEffective := "CapInh:\t0\nCapPrm:\t1000\nCapEff:\t0\nCapBnd:\t1000\n"
	notBounding := "CapInh:\t0\nCapPrm:\t1000\nCapEff:\t1000\nCapBnd:\t0\n"
	tests := []struct {
		name     string
		statuses []string
		want     bool
		wantErr  bool
	}{
		{"every thread usable", []string{usable, usable}, true, false},
		{"one thread lacks effective", []string{usable, notEffective}, false, false},
		{"one thread lacks bounding", []string{notBounding, usable}, false, false},
		{"unparsable status", []string{usable, "CapEff:\t1000\n"}, false, true},
		{"no threads", nil, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := allUsable(tt.statuses, capNetAdmin)
			if (err != nil) != tt.wantErr {
				t.Fatalf("allUsable() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("allUsable() = %v, want %v", got, tt.want)
			}
		})
	}
}
