package agent

import (
	"fmt"
	"log/slog"
	"sort"
)

// Hub env authority for broker-mode starts.
//
// For an agent started through the broker API (opts.BrokerMode, which today
// means hub-dispatched), some env keys may come only from the hub: the hub
// sends them in opts.Env when it resolves a value, and the broker never fills
// them from its own layers. Without this rule a broker-local layer could keep
// supplying a value the hub no longer reports, such as a TZ persisted in
// scion-agent.json after the hub was told to unpin it.
//
// The broker-local layers that are skipped:
//   - the config layer (finalScionCfg.Env: the persisted scion-agent.json,
//     broker-local templates and the harness-config directory env), in
//     buildAgentEnv;
//   - the broker settings' harness-config entry env, in resolveAuthEnvOverlay.
//
// Nothing on disk is modified: finalScionCfg is not mutated and
// scion-agent.json is not scrubbed, so the rule is reversible and a local
// (non-broker) start still sees the on-disk value. Solo mode is unaffected.

// hubOnlyEnvKeys lists the env keys that only the hub may supply for a
// broker-mode start.
var hubOnlyEnvKeys = map[string]struct{}{
	"TZ": {},
}

// Layer names reported for a dropped broker-local value.
const (
	envLayerConfig             = "config"
	envLayerHarnessConfigEntry = "harness-config entry"
)

// IsHubOnlyEnvKey reports whether key may be supplied only by the hub for a
// hub-dispatched agent. The broker never treats such a key as a required
// (gathered) env key.
func IsHubOnlyEnvKey(key string) bool {
	_, ok := hubOnlyEnvKeys[key]
	return ok
}

// skipBrokerLocalEnvKey reports whether key must be skipped from broker-local layers
// for a start in the given mode.
func skipBrokerLocalEnvKey(brokerMode bool, key string) bool {
	return brokerMode && IsHubOnlyEnvKey(key)
}

// droppedBrokerEnv records a broker-local value that was skipped because only
// the hub may supply its key.
type droppedBrokerEnv struct {
	Key   string
	Value string
	Layer string
}

func sortDroppedBrokerEnv(dropped []droppedBrokerEnv) {
	sort.Slice(dropped, func(i, j int) bool {
		if dropped[i].Layer != dropped[j].Layer {
			return dropped[i].Layer < dropped[j].Layer
		}
		return dropped[i].Key < dropped[j].Key
	})
}

// warnDroppedBrokerEnv logs each dropped non-empty value at warn level and
// returns matching start warnings. An empty value is a "no value" marker, so
// dropping it changes nothing worth reporting. A value equal to what the hub
// supplied in hubEnv is not reported either: the container gets that value
// anyway.
func warnDroppedBrokerEnv(agentID string, hubEnv map[string]string, dropped []droppedBrokerEnv) []string {
	var warnings []string
	for _, d := range dropped {
		if d.Value == "" {
			continue
		}
		hubValue, hubSet := hubEnv[d.Key]
		if hubSet && hubValue == d.Value {
			continue
		}
		slog.Warn("agent start: ignoring broker-local env value; only the hub supplies this key for hub-dispatched agents",
			"agent_id", agentID, "key", d.Key, "value", d.Value, "layer", d.Layer, "hub_supplied", hubSet && hubValue != "")
		warnings = append(warnings, fmt.Sprintf(
			"Warning: ignoring %s=%q from the broker %s layer for agent %s: only the hub supplies %s for hub-dispatched agents (set it as a hub environment variable instead)",
			d.Key, d.Value, d.Layer, agentID, d.Key))
	}
	return warnings
}
