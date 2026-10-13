package runtimebroker

import (
	"reflect"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// scionConfigFieldIndexByJSONKey indexes api.ScionConfig's fields by json key.
var scionConfigFieldIndexByJSONKey = func() map[string]int {
	m := map[string]int{}
	typ := reflect.TypeOf(api.ScionConfig{})
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			m[name] = i
		}
	}
	return m
}()

// applyClearedFields removes each cleared path from cfg so the inherited
// value applies (ptone/scion#3906). It runs on the agent's persisted config
// after the hub's inline config has been merged over it, and before the
// start reads that config (provisioning and the harness provisioners among
// them), so a cleared value is never handed to the agent again. A path
// ClearableConfigPath does not accept is ignored, as is one already absent,
// so applying the same list on every start is idempotent. It returns the
// paths it ignored.
//
// "Inherited" is what the start re-derives under the persisted config:
// GetAgent merges the template chain again whenever the template resolves
// by name, so a removed key falls back to the template's value. Known gap:
// an agent whose template was loaded from a content-addressed cache does
// not resolve its template by name at start, so a removed key falls back
// to the broker and harness defaults instead, until the agent is
// reincarnated.
func applyClearedFields(cfg *api.ScionConfig, paths []string) (ignored []string) {
	if cfg == nil {
		return nil
	}
	v := reflect.ValueOf(cfg).Elem()
	for _, p := range paths {
		if !api.ClearableConfigPath(p) {
			ignored = append(ignored, p)
			continue
		}
		if k, ok := strings.CutPrefix(p, api.ClearedEnvPathPrefix); ok {
			delete(cfg.Env, k)
			continue
		}
		i, ok := scionConfigFieldIndexByJSONKey[p]
		if !ok {
			ignored = append(ignored, p)
			continue
		}
		f := v.Field(i)
		f.Set(reflect.Zero(f.Type()))
	}
	return ignored
}

// dedupeVolumesByTarget collapses volumes that mount at the same target,
// keeping the first entry's position and the last entry's value, so a
// later entry (the hub's inline config, merged after the persisted config)
// replaces an earlier one for the same target instead of mounting twice.
// config.MergeScionConfig appends volumes, and the hub sends the agent's
// inline volumes on every start, so without this they grow with each start
// (ptone/scion#3906). Keying on the target, not the source, lets an
// override of a default mount replace it. Entries with no target are kept
// as they are.
func dedupeVolumesByTarget(vols []api.VolumeMount) []api.VolumeMount {
	if len(vols) < 2 {
		return vols
	}
	pos := make(map[string]int, len(vols))
	out := make([]api.VolumeMount, 0, len(vols))
	for _, vol := range vols {
		if vol.Target == "" {
			out = append(out, vol)
			continue
		}
		if i, ok := pos[vol.Target]; ok {
			out[i] = vol
			continue
		}
		pos[vol.Target] = len(out)
		out = append(out, vol)
	}
	return out
}
