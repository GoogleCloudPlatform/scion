/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"errors"
	"reflect"
	"testing"
)

func withNetnsSetupSeams(t *testing.T, euid int, install func(mode string, port int) error) {
	t.Helper()
	origEUID, origInstall := netnsSetupEUID, netnsSetupInstallRules
	netnsSetupEUID = func() int { return euid }
	netnsSetupInstallRules = install
	t.Cleanup(func() {
		netnsSetupEUID, netnsSetupInstallRules = origEUID, origInstall
	})
}

func TestRunNetnsSetup_RefusesNonRoot(t *testing.T) {
	t.Setenv("SCION_METADATA_MODE", "block")
	var called bool
	withNetnsSetupSeams(t, 1000, func(string, int) error {
		called = true
		return nil
	})
	if err := runNetnsSetup(); err == nil {
		t.Fatal("runNetnsSetup() = nil for non-root, want error")
	}
	if called {
		t.Error("InstallInterceptionRules called when not root")
	}
}

func TestRunNetnsSetup_RefusesMissingOrPassthroughMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		set  bool
	}{
		{name: "unset", set: false},
		{name: "passthrough", mode: "passthrough", set: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("SCION_METADATA_MODE", tc.mode)
			} else {
				unsetEnvForTest(t, "SCION_METADATA_MODE")
			}
			var called bool
			withNetnsSetupSeams(t, 0, func(string, int) error {
				called = true
				return nil
			})
			if err := runNetnsSetup(); err == nil {
				t.Fatal("runNetnsSetup() = nil, want error")
			}
			if called {
				t.Error("InstallInterceptionRules called without valid metadata mode")
			}
		})
	}
}

func TestRunNetnsSetup_FirewallBeforeRules(t *testing.T) {
	t.Setenv("SCION_METADATA_MODE", "block")
	t.Setenv("SCION_METADATA_PORT", "18381")
	t.Setenv(initFirewallEnv, "true")

	var order []string
	var gotMode string
	var gotPort int
	withRunInitFirewall(t, func() error {
		order = append(order, "firewall")
		return nil
	})
	withNetnsSetupSeams(t, 0, func(mode string, port int) error {
		order = append(order, "rules")
		gotMode = mode
		gotPort = port
		return nil
	})

	if err := runNetnsSetup(); err != nil {
		t.Fatalf("runNetnsSetup() = %v, want nil", err)
	}
	if want := []string{"firewall", "rules"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if gotMode != "block" || gotPort != 18381 {
		t.Errorf("InstallInterceptionRules(%q, %d), want (%q, %d)", gotMode, gotPort, "block", 18381)
	}
}

func TestRunNetnsSetup_FirewallFailureSkipsRules(t *testing.T) {
	t.Setenv("SCION_METADATA_MODE", "assign")
	t.Setenv(initFirewallEnv, "true")

	withRunInitFirewall(t, func() error {
		return errors.New("firewall failed")
	})
	var rulesCalled bool
	withNetnsSetupSeams(t, 0, func(string, int) error {
		rulesCalled = true
		return nil
	})

	if err := runNetnsSetup(); err == nil {
		t.Fatal("runNetnsSetup() = nil on firewall failure, want error")
	}
	if rulesCalled {
		t.Error("InstallInterceptionRules ran after firewall failure")
	}
}

func TestRunNetnsSetup_RulesFailureReturnsError(t *testing.T) {
	t.Setenv("SCION_METADATA_MODE", "assign")
	t.Setenv(initFirewallEnv, "")

	wantErr := errors.New("iptables failed")
	withNetnsSetupSeams(t, 0, func(string, int) error {
		return wantErr
	})

	if err := runNetnsSetup(); !errors.Is(err, wantErr) {
		t.Fatalf("runNetnsSetup() = %v, want %v", err, wantErr)
	}
}
