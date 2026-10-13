package api

import "strings"

// ClearedEnvPathPrefix prefixes a ScionConfig.ClearedFields entry that names
// one env key: "env.FOO" clears the agent's own value for FOO.
const ClearedEnvPathPrefix = "env."

// clearableConfigKeys are the top-level ScionConfig json keys a
// ClearedFields entry may name: the scalar and struct keys whose cleared
// value a start would otherwise keep. Collections (env, volumes,
// mcp_servers, skills, services) are not cleared wholesale: env is cleared
// per key (ClearedEnvPathPrefix), and removing a whole volume list could
// drop mounts the broker added itself, such as the workspace. Telemetry is
// left out: its TLS cert and key files must clear together.
var clearableConfigKeys = map[string]bool{
	"model":             true,
	"thinking_level":    true,
	"auth_selectedType": true,
	"image":             true,
	"user":              true,
	"max_turns":         true,
	"max_model_calls":   true,
	"max_duration":      true,
	"resources":         true,
	"kubernetes":        true,
	"command_args":      true,
	"task_flag":         true,
	"task":              true,
	"system_prompt":     true,
}

// ClearableConfigPath reports whether path is a ScionConfig.ClearedFields
// entry a Runtime Broker applies: a key in clearableConfigKeys, or
// ClearedEnvPathPrefix followed by a non-empty env key.
func ClearableConfigPath(path string) bool {
	if k, ok := strings.CutPrefix(path, ClearedEnvPathPrefix); ok {
		return k != ""
	}
	return clearableConfigKeys[path]
}
