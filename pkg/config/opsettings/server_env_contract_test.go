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

package opsettings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Env contract test (ptone/scion#3910).
//
// For every server setting (each leaf registry KoanfPath and each leaf under
// a Layer-0 prefix) the canonical env spelling is the settings schema's
// x-env-var name when it is a SCION_SERVER_ name. Otherwise it is derived
// from the koanf path: drop a leading "server.", remove the underscores
// inside each segment, upper-case, join the segments with "_", and prefix
// SCION_SERVER_ (server.hub.admin_emails -> SCION_SERVER_HUB_ADMINEMAILS).
// TestSchemaEnvVarDerivation keeps the derivation and the schema aligned.
// The seed spelling puts SCION_SEED_ in front of the server path
// (SCION_SEED_SERVER_HUB_ADMINEMAILS).
//
// The test sets that spelling and checks it reaches the key through every
// env consumer that has a field for it, and that an exported but empty
// value is treated as unset. Current gaps are listed in envContractGaps;
// the single mapping table (ptone/scion#3911) drains that list.

// Env consumers checked by the contract test.
const (
	// consumerOpsettings is LoadEnvKoanf (serverEnvToOpsettingsKey: the
	// snakeCaseFields and serverSubKeys mapper). It feeds the opsettings
	// env layer, the bootstrap merge and the admin UI env badges. Every key
	// is checked.
	consumerOpsettings = "opsettings"
	// consumerSeed is LoadSeedEnvKoanf (envKeyToOpsettingsKey) with the
	// SCION_SEED_ spelling. Seed values only seed Layer-1 keys, so only
	// Layer-1 keys are checked.
	consumerSeed = "seed"
	// consumerGlobalConfig is LoadGlobalConfig (envKeyToConfigKey: the
	// camelCaseFields mapper), on both the settings.yaml and the legacy
	// server.yaml load path. Checked for keys GlobalConfig has a field for.
	consumerGlobalConfig = "globalconfig"
	// consumerVersioned is LoadVersionedSettings (versionedEnvKeyMapper:
	// the SCION_SERVER_* branch). Checked for server.* keys
	// VersionedSettings has a field for.
	consumerVersioned = "versioned"
)

var envContractConsumers = []string{consumerOpsettings, consumerSeed, consumerGlobalConfig, consumerVersioned}

// Reasons used in envContractGaps.
const (
	gapSnakeCaseFields = "snakeCaseFields has no entry for a compound segment of the key, so the canonical " +
		"spelling lands on an unsplit key and the key is never set (ptone/scion#3911)"
	gapGlobalConfigBroker = "GlobalConfig names the section runtimeBroker, so only SCION_SERVER_RUNTIMEBROKER_* " +
		"reaches it, not SCION_SERVER_BROKER_* (ptone/scion#3911)"
	gapGlobalConfigCORS = "GlobalConfig flattens cors.* into corsEnabled, corsMaxAge, ...; only the flattened " +
		"spelling (SCION_SERVER_HUB_CORSENABLED) reaches it (ptone/scion#3911)"
	gapGlobalConfigCamel = "camelCaseFields has no entry for a compound segment of the key, so the canonical " +
		"spelling lands on a lower-case key that matches no field (ptone/scion#3911)"
	gapVersionedUnderscored = "versionedEnvKeyMapper expects the underscored spelling of a compound field " +
		"(SCION_SERVER_HUB_ADMIN_EMAILS); the canonical spelling binds to no field (ptone/scion#3911)"
	gapVersionedSection = "versionedEnvKeyMapper does not treat a segment of the key as a section, so the " +
		"canonical spelling is not split and binds to no field (ptone/scion#3911)"
	gapRuntimeBrokerName = "the schema name is SCION_SERVER_RUNTIMEBROKER_*, which this consumer maps under a " +
		"runtimebroker section it does not have; only GlobalConfig has runtimeBroker (ptone/scion#3911)"
	gapUnderscoredName = "the schema name keeps the field's underscores (SCION_SERVER_BROKER_BROKER_ID), which " +
		"this consumer splits into extra segments (ptone/scion#3911)"
	gapHubEndpointName = "the schema name is SCION_SERVER_HUB_ENDPOINT, which this consumer maps to " +
		"server.hub.endpoint, not server.hub.public_url (ptone/scion#3911)"
)

// envContractGaps lists the known (consumer, key) pairs where the canonical
// spelling does not reach the key. Keyed "<consumer> <koanf path>". The list
// may only shrink: a new gap fails the test, and so does an entry whose gap
// has been fixed (remove it). ptone/scion#3911 drains it.
var envContractGaps = map[string]string{
	// opsettings
	"opsettings default_agent_role":                                       gapSnakeCaseFields,
	"opsettings default_gcp_identity_mode":                                gapSnakeCaseFields,
	"opsettings default_gcp_identity_service_account_id":                  gapSnakeCaseFields,
	"opsettings default_max_agent_role":                                   gapSnakeCaseFields,
	"opsettings default_model":                                            gapSnakeCaseFields,
	"opsettings default_runtime_broker":                                   gapSnakeCaseFields,
	"opsettings default_thinking_level":                                   gapSnakeCaseFields,
	"opsettings default_timezone":                                         gapSnakeCaseFields,
	"opsettings server.auth.proxy.iap.jwks_url":                           gapSnakeCaseFields,
	"opsettings server.auth.proxy.jwt.claims.display_name":                gapSnakeCaseFields,
	"opsettings server.auth.proxy.jwt.jwks_file":                          gapSnakeCaseFields,
	"opsettings server.auth.proxy.jwt.jwks_url":                           gapSnakeCaseFields,
	"opsettings server.auth.proxy.jwt.public_key_file":                    gapSnakeCaseFields,
	"opsettings server.auth.proxy.require_trusted_proxy_ip":               gapSnakeCaseFields,
	"opsettings server.auth.transport.oidc_audience":                      gapSnakeCaseFields,
	"opsettings server.auth.transport.platform_auth_sa":                   gapSnakeCaseFields,
	"opsettings server.broker.allow_container_script_harnesses":           gapRuntimeBrokerName,
	"opsettings server.broker.auto_provide":                               gapUnderscoredName,
	"opsettings server.broker.broker_id":                                  gapUnderscoredName,
	"opsettings server.broker.broker_name":                                gapUnderscoredName,
	"opsettings server.broker.broker_nickname":                            gapUnderscoredName,
	"opsettings server.broker.broker_token":                               gapUnderscoredName,
	"opsettings server.broker.container_hub_endpoint":                     gapRuntimeBrokerName,
	"opsettings server.broker.cors.allowed_headers":                       gapSnakeCaseFields,
	"opsettings server.broker.cors.allowed_methods":                       gapSnakeCaseFields,
	"opsettings server.broker.cors.allowed_origins":                       gapSnakeCaseFields,
	"opsettings server.broker.cors.max_age":                               gapSnakeCaseFields,
	"opsettings server.broker.enabled":                                    gapRuntimeBrokerName,
	"opsettings server.broker.host":                                       gapRuntimeBrokerName,
	"opsettings server.broker.hub_endpoint":                               gapRuntimeBrokerName,
	"opsettings server.broker.port":                                       gapRuntimeBrokerName,
	"opsettings server.broker.read_timeout":                               gapRuntimeBrokerName,
	"opsettings server.broker.write_timeout":                              gapRuntimeBrokerName,
	"opsettings server.database.conn_max_idle_time":                       gapSnakeCaseFields,
	"opsettings server.database.conn_max_lifetime":                        gapSnakeCaseFields,
	"opsettings server.database.max_idle_conns":                           gapSnakeCaseFields,
	"opsettings server.database.max_open_conns":                           gapSnakeCaseFields,
	"opsettings server.home_storage.allow_incomplete_phases":              gapSnakeCaseFields,
	"opsettings server.home_storage.skeleton_max_bytes":                   gapSnakeCaseFields,
	"opsettings server.home_storage.stop_grace_seconds":                   gapSnakeCaseFields,
	"opsettings server.home_storage.termination_wait_seconds":             gapSnakeCaseFields,
	"opsettings server.hub.async_agent_launch":                            gapSnakeCaseFields,
	"opsettings server.hub.cors.allowed_headers":                          gapSnakeCaseFields,
	"opsettings server.hub.cors.allowed_methods":                          gapSnakeCaseFields,
	"opsettings server.hub.cors.allowed_origins":                          gapSnakeCaseFields,
	"opsettings server.hub.cors.max_age":                                  gapSnakeCaseFields,
	"opsettings server.hub.launch_keepalive_seconds":                      gapSnakeCaseFields,
	"opsettings server.hub.launch_timeout":                                gapSnakeCaseFields,
	"opsettings server.hub.perf_trace":                                    gapSnakeCaseFields,
	"opsettings server.hub.public_url":                                    gapHubEndpointName,
	"opsettings server.oauth.cli.github.client_id":                        gapSnakeCaseFields,
	"opsettings server.oauth.cli.github.client_secret":                    gapSnakeCaseFields,
	"opsettings server.oauth.cli.google.client_id":                        gapSnakeCaseFields,
	"opsettings server.oauth.cli.google.client_secret":                    gapSnakeCaseFields,
	"opsettings server.oauth.device.github.client_id":                     gapSnakeCaseFields,
	"opsettings server.oauth.device.github.client_secret":                 gapSnakeCaseFields,
	"opsettings server.oauth.device.google.client_id":                     gapSnakeCaseFields,
	"opsettings server.oauth.device.google.client_secret":                 gapSnakeCaseFields,
	"opsettings server.oauth.web.github.client_id":                        gapSnakeCaseFields,
	"opsettings server.oauth.web.github.client_secret":                    gapSnakeCaseFields,
	"opsettings server.oauth.web.google.client_id":                        gapSnakeCaseFields,
	"opsettings server.oauth.web.google.client_secret":                    gapSnakeCaseFields,
	"opsettings server.secrets.gcp_credentials":                           gapSnakeCaseFields,
	"opsettings server.shared_dir_storage.nfs.mount_options":              gapSnakeCaseFields,
	"opsettings server.shared_dir_storage.nfs.mount_root":                 gapSnakeCaseFields,
	"opsettings server.shared_dir_storage.nfs.storage_class":              gapSnakeCaseFields,
	"opsettings server.shared_dir_storage.nfs.subpath_root":               gapSnakeCaseFields,
	"opsettings server.storage.local_path":                                gapSnakeCaseFields,
	"opsettings server.workspace_storage.cloudrun_volume.subpath_root":    gapSnakeCaseFields,
	"opsettings server.workspace_storage.cloudrun_volume.volume_name":     gapSnakeCaseFields,
	"opsettings server.workspace_storage.gke_shared_volume.pv_claim_name": gapSnakeCaseFields,
	"opsettings server.workspace_storage.gke_shared_volume.subpath_root":  gapSnakeCaseFields,
	"opsettings server.workspace_storage.gke_shared_volume.volume_name":   gapSnakeCaseFields,
	"opsettings server.workspace_storage.nfs.auto_mount":                  gapSnakeCaseFields,
	"opsettings server.workspace_storage.nfs.mount_options":               gapSnakeCaseFields,
	"opsettings server.workspace_storage.nfs.mount_root":                  gapSnakeCaseFields,
	"opsettings server.workspace_storage.nfs.storage_class":               gapSnakeCaseFields,
	"opsettings server.workspace_storage.nfs.subpath_root":                gapSnakeCaseFields,

	// seed
	"seed default_agent_role":                      gapSnakeCaseFields,
	"seed default_gcp_identity_mode":               gapSnakeCaseFields,
	"seed default_gcp_identity_service_account_id": gapSnakeCaseFields,
	"seed default_max_agent_role":                  gapSnakeCaseFields,
	"seed default_model":                           gapSnakeCaseFields,
	"seed default_runtime_broker":                  gapSnakeCaseFields,
	"seed default_thinking_level":                  gapSnakeCaseFields,
	"seed default_timezone":                        gapSnakeCaseFields,
	"seed server.hub.public_url":                   gapHubEndpointName,

	// globalconfig
	"globalconfig server.broker.broker_id":            gapUnderscoredName,
	"globalconfig server.broker.broker_name":          gapUnderscoredName,
	"globalconfig server.broker.cors.allowed_headers": gapGlobalConfigBroker,
	"globalconfig server.broker.cors.allowed_methods": gapGlobalConfigBroker,
	"globalconfig server.broker.cors.allowed_origins": gapGlobalConfigBroker,
	"globalconfig server.broker.cors.enabled":         gapGlobalConfigBroker,
	"globalconfig server.broker.cors.max_age":         gapGlobalConfigBroker,
	"globalconfig server.database.conn_max_idle_time": gapGlobalConfigCamel,
	"globalconfig server.database.conn_max_lifetime":  gapGlobalConfigCamel,
	"globalconfig server.database.max_idle_conns":     gapGlobalConfigCamel,
	"globalconfig server.database.max_open_conns":     gapGlobalConfigCamel,
	"globalconfig server.hub.cors.allowed_headers":    gapGlobalConfigCORS,
	"globalconfig server.hub.cors.allowed_methods":    gapGlobalConfigCORS,
	"globalconfig server.hub.cors.allowed_origins":    gapGlobalConfigCORS,
	"globalconfig server.hub.cors.enabled":            gapGlobalConfigCORS,
	"globalconfig server.hub.cors.max_age":            gapGlobalConfigCORS,

	// versioned
	"versioned server.auth.authorized_domains":                           gapVersionedUnderscored,
	"versioned server.auth.default_user_role":                            gapVersionedUnderscored,
	"versioned server.auth.dev_mode":                                     gapVersionedUnderscored,
	"versioned server.auth.dev_token":                                    gapVersionedUnderscored,
	"versioned server.auth.dev_token_file":                               gapVersionedUnderscored,
	"versioned server.auth.proxy.iap.jwks_url":                           gapVersionedUnderscored,
	"versioned server.auth.proxy.jwt.claims.display_name":                gapVersionedUnderscored,
	"versioned server.auth.proxy.jwt.jwks_file":                          gapVersionedUnderscored,
	"versioned server.auth.proxy.jwt.jwks_url":                           gapVersionedUnderscored,
	"versioned server.auth.proxy.jwt.public_key_file":                    gapVersionedUnderscored,
	"versioned server.auth.proxy.require_trusted_proxy_ip":               gapVersionedUnderscored,
	"versioned server.auth.transport.oidc_audience":                      gapVersionedUnderscored,
	"versioned server.auth.transport.platform_auth_sa":                   gapVersionedUnderscored,
	"versioned server.auth.user_access_mode":                             gapVersionedUnderscored,
	"versioned server.broker.allow_container_script_harnesses":           gapRuntimeBrokerName,
	"versioned server.broker.container_hub_endpoint":                     gapRuntimeBrokerName,
	"versioned server.broker.cors.allowed_headers":                       gapVersionedUnderscored,
	"versioned server.broker.cors.allowed_methods":                       gapVersionedUnderscored,
	"versioned server.broker.cors.allowed_origins":                       gapVersionedUnderscored,
	"versioned server.broker.cors.max_age":                               gapVersionedUnderscored,
	"versioned server.broker.enabled":                                    gapRuntimeBrokerName,
	"versioned server.broker.host":                                       gapRuntimeBrokerName,
	"versioned server.broker.hub_endpoint":                               gapRuntimeBrokerName,
	"versioned server.broker.port":                                       gapRuntimeBrokerName,
	"versioned server.broker.read_timeout":                               gapRuntimeBrokerName,
	"versioned server.broker.write_timeout":                              gapRuntimeBrokerName,
	"versioned server.database.conn_max_idle_time":                       gapVersionedUnderscored,
	"versioned server.database.conn_max_lifetime":                        gapVersionedUnderscored,
	"versioned server.database.max_idle_conns":                           gapVersionedUnderscored,
	"versioned server.database.max_open_conns":                           gapVersionedUnderscored,
	"versioned server.federation.algorithms":                             gapVersionedSection,
	"versioned server.federation.debounce_interval":                      gapVersionedUnderscored,
	"versioned server.federation.enabled":                                gapVersionedSection,
	"versioned server.federation.refresh_interval":                       gapVersionedUnderscored,
	"versioned server.github_app.api_base_url":                           gapVersionedUnderscored,
	"versioned server.github_app.app_id":                                 gapVersionedUnderscored,
	"versioned server.github_app.installation_url":                       gapVersionedUnderscored,
	"versioned server.github_app.private_key_path":                       gapVersionedUnderscored,
	"versioned server.github_app.webhooks_enabled":                       gapVersionedUnderscored,
	"versioned server.home_storage.allow_incomplete_phases":              gapVersionedUnderscored,
	"versioned server.home_storage.backend":                              gapVersionedUnderscored,
	"versioned server.home_storage.leaf":                                 gapVersionedUnderscored,
	"versioned server.home_storage.skeleton_max_bytes":                   gapVersionedUnderscored,
	"versioned server.home_storage.stop_grace_seconds":                   gapVersionedUnderscored,
	"versioned server.home_storage.termination_wait_seconds":             gapVersionedUnderscored,
	"versioned server.hub.admin_emails":                                  gapVersionedUnderscored,
	"versioned server.hub.async_agent_launch":                            gapVersionedUnderscored,
	"versioned server.hub.auto_suspend_stalled":                          gapVersionedUnderscored,
	"versioned server.hub.conduit.authz_recheck_interval":                gapVersionedUnderscored,
	"versioned server.hub.conduit.grant_key_activation":                  gapVersionedUnderscored,
	"versioned server.hub.conduit.instance_id":                           gapVersionedUnderscored,
	"versioned server.hub.conduit.internal_advertise":                    gapVersionedUnderscored,
	"versioned server.hub.conduit.internal_listen":                       gapVersionedUnderscored,
	"versioned server.hub.conduit.lifetime_cap":                          gapVersionedUnderscored,
	"versioned server.hub.conduit.peer_audience":                         gapVersionedUnderscored,
	"versioned server.hub.conduit.peer_auth":                             gapVersionedUnderscored,
	"versioned server.hub.conduit.peer_service_accounts":                 gapVersionedUnderscored,
	"versioned server.hub.conduit.proxy_session_max_age":                 gapVersionedUnderscored,
	"versioned server.hub.conduit.reconnect_window":                      gapVersionedUnderscored,
	"versioned server.hub.conduit.stream_authz_max.agent":                gapVersionedUnderscored,
	"versioned server.hub.conduit.stream_authz_max.broker":               gapVersionedUnderscored,
	"versioned server.hub.conduit.stream_authz_max.user":                 gapVersionedUnderscored,
	"versioned server.hub.conduit.tcp_allowed_ports":                     gapVersionedUnderscored,
	"versioned server.hub.cors.allowed_headers":                          gapVersionedUnderscored,
	"versioned server.hub.cors.allowed_methods":                          gapVersionedUnderscored,
	"versioned server.hub.cors.allowed_origins":                          gapVersionedUnderscored,
	"versioned server.hub.cors.max_age":                                  gapVersionedUnderscored,
	"versioned server.hub.gcp_iam_check_mode":                            gapVersionedUnderscored,
	"versioned server.hub.gcp_iam_deny_unknown_policy":                   gapVersionedUnderscored,
	"versioned server.hub.hub_id":                                        gapVersionedUnderscored,
	"versioned server.hub.hub_name":                                      gapVersionedUnderscored,
	"versioned server.hub.launch_keepalive_seconds":                      gapVersionedUnderscored,
	"versioned server.hub.launch_timeout":                                gapVersionedUnderscored,
	"versioned server.hub.membership_sweep_report_only":                  gapVersionedUnderscored,
	"versioned server.hub.missing_agent_grace":                           gapVersionedUnderscored,
	"versioned server.hub.monitoring_dashboard_url":                      gapVersionedUnderscored,
	"versioned server.hub.perf_trace":                                    gapVersionedUnderscored,
	"versioned server.hub.port_proxy.response_header_timeout":            gapVersionedUnderscored,
	"versioned server.hub.public_url":                                    gapHubEndpointName,
	"versioned server.hub.read_timeout":                                  gapVersionedUnderscored,
	"versioned server.hub.soft_delete_retain_files":                      gapVersionedUnderscored,
	"versioned server.hub.soft_delete_retention":                         gapVersionedUnderscored,
	"versioned server.hub.stalled_threshold":                             gapVersionedUnderscored,
	"versioned server.hub.start_claim_lease_ttl":                         gapVersionedUnderscored,
	"versioned server.hub.start_create_unconfirmed_hold":                 gapVersionedUnderscored,
	"versioned server.hub.start_max_duration":                            gapVersionedUnderscored,
	"versioned server.hub.start_unconfirmed_hold":                        gapVersionedUnderscored,
	"versioned server.hub.write_timeout":                                 gapVersionedUnderscored,
	"versioned server.log_format":                                        gapVersionedUnderscored,
	"versioned server.log_level":                                         gapVersionedUnderscored,
	"versioned server.message_broker.enabled":                            gapVersionedUnderscored,
	"versioned server.message_broker.type":                               gapVersionedUnderscored,
	"versioned server.message_broker.types":                              gapVersionedUnderscored,
	"versioned server.native_chat.enabled":                               gapVersionedUnderscored,
	"versioned server.oauth.cli.github.client_id":                        gapVersionedUnderscored,
	"versioned server.oauth.cli.github.client_secret":                    gapVersionedUnderscored,
	"versioned server.oauth.cli.google.client_id":                        gapVersionedUnderscored,
	"versioned server.oauth.cli.google.client_secret":                    gapVersionedUnderscored,
	"versioned server.oauth.device.github.client_id":                     gapVersionedUnderscored,
	"versioned server.oauth.device.github.client_secret":                 gapVersionedUnderscored,
	"versioned server.oauth.device.google.client_id":                     gapVersionedUnderscored,
	"versioned server.oauth.device.google.client_secret":                 gapVersionedUnderscored,
	"versioned server.oauth.web.github.client_id":                        gapVersionedUnderscored,
	"versioned server.oauth.web.github.client_secret":                    gapVersionedUnderscored,
	"versioned server.oauth.web.google.client_id":                        gapVersionedUnderscored,
	"versioned server.oauth.web.google.client_secret":                    gapVersionedUnderscored,
	"versioned server.secrets.gcp_credentials":                           gapVersionedUnderscored,
	"versioned server.secrets.gcp_project_id":                            gapVersionedUnderscored,
	"versioned server.secrets.gcp_replication_locations":                 gapVersionedUnderscored,
	"versioned server.shared_dir_storage.backend":                        gapVersionedUnderscored,
	"versioned server.shared_dir_storage.nfs.gid":                        gapVersionedUnderscored,
	"versioned server.shared_dir_storage.nfs.mount_options":              gapVersionedUnderscored,
	"versioned server.shared_dir_storage.nfs.mount_root":                 gapVersionedUnderscored,
	"versioned server.shared_dir_storage.nfs.storage_class":              gapVersionedUnderscored,
	"versioned server.shared_dir_storage.nfs.subpath_root":               gapVersionedUnderscored,
	"versioned server.shared_dir_storage.nfs.uid":                        gapVersionedUnderscored,
	"versioned server.storage.local_path":                                gapVersionedUnderscored,
	"versioned server.workspace_storage.backend":                         gapVersionedUnderscored,
	"versioned server.workspace_storage.cloudrun_volume.subpath_root":    gapVersionedUnderscored,
	"versioned server.workspace_storage.cloudrun_volume.volume_name":     gapVersionedUnderscored,
	"versioned server.workspace_storage.gke_shared_volume.pv_claim_name": gapVersionedUnderscored,
	"versioned server.workspace_storage.gke_shared_volume.subpath_root":  gapVersionedUnderscored,
	"versioned server.workspace_storage.gke_shared_volume.volume_name":   gapVersionedUnderscored,
	"versioned server.workspace_storage.nfs.auto_mount":                  gapVersionedUnderscored,
	"versioned server.workspace_storage.nfs.gid":                         gapVersionedUnderscored,
	"versioned server.workspace_storage.nfs.mount_options":               gapVersionedUnderscored,
	"versioned server.workspace_storage.nfs.mount_root":                  gapVersionedUnderscored,
	"versioned server.workspace_storage.nfs.storage_class":               gapVersionedUnderscored,
	"versioned server.workspace_storage.nfs.subpath_root":                gapVersionedUnderscored,
	"versioned server.workspace_storage.nfs.uid":                         gapVersionedUnderscored,
}

// envContractStructuredKeys lists keys whose value is a map or a list of
// objects. No single env string can set them, so they are outside the
// contract. The test checks this list against the schema in both directions.
var envContractStructuredKeys = map[string]bool{
	"default_resources":                    true, // object
	"harness_configs":                      true, // map of objects
	"profiles":                             true, // map of objects
	"runtimes":                             true, // map of objects
	"server.broker.instances":              true, // list of objects
	"server.federation.trusted_issuers":    true, // list of objects (ptone/scion#3836)
	"server.notification_channels":         true, // list of objects
	"server.plugins.broker":                true, // map of objects
	"server.shared_dir_storage.nfs.shares": true, // list of objects
	"server.workspace_storage.nfs.shares":  true, // list of objects
	"telemetry.cloud.headers":              true, // map
	"telemetry.filter.sampling.rates":      true, // map
	"telemetry.resource":                   true, // map
}

// schemaNode is a settings-v1 schema property with local $refs merged in.
type schemaNode map[string]interface{}

// collectSchemaNodes walks settings-v1.schema.json and returns every
// property node by dotted path, with a $ref's definition merged under the
// node's own keywords.
func collectSchemaNodes(t *testing.T) map[string]schemaNode {
	t.Helper()
	data, err := config.GetSettingsSchemaJSON("1")
	if err != nil {
		t.Fatalf("read settings schema: %v", err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse settings schema: %v", err)
	}
	defs, _ := root["$defs"].(map[string]interface{})
	out := map[string]schemaNode{}
	var walk func(node map[string]interface{}, path string, depth int)
	walk = func(node map[string]interface{}, path string, depth int) {
		if depth > 25 {
			return
		}
		if ref, ok := node["$ref"].(string); ok && strings.HasPrefix(ref, "#/$defs/") {
			if def, ok := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]interface{}); ok {
				merged := map[string]interface{}{}
				for k, v := range def {
					merged[k] = v
				}
				for k, v := range node {
					if k != "$ref" {
						merged[k] = v
					}
				}
				node = merged
			}
		}
		if path != "" {
			out[path] = node
		}
		props, _ := node["properties"].(map[string]interface{})
		for name, child := range props {
			if cm, ok := child.(map[string]interface{}); ok {
				p := name
				if path != "" {
					p = path + "." + name
				}
				walk(cm, p, depth+1)
			}
		}
	}
	walk(root, "", 0)
	return out
}

func (n schemaNode) hasProperties() bool {
	_, ok := n["properties"]
	return ok
}

// typ returns the node's JSON type, ignoring "null" in a type list
// (["array", "null"] is "array").
func (n schemaNode) typ() string {
	switch v := n["type"].(type) {
	case string:
		return v
	case []interface{}:
		for _, t := range v {
			if s, ok := t.(string); ok && s != "null" {
				return s
			}
		}
	}
	return ""
}

// isStructured reports whether a leaf node holds a map or a list of objects.
func (n schemaNode) isStructured() bool {
	switch n.typ() {
	case "object":
		return true
	case "array":
		items, _ := n["items"].(map[string]interface{})
		if items == nil {
			return false
		}
		if items["type"] == "object" {
			return true
		}
		_, hasProps := items["properties"]
		_, hasRef := items["$ref"]
		return hasProps || hasRef
	}
	return false
}

// entry converts a node to the shape sampleEnvValues takes.
func (n schemaNode) entry(path string) schemaEnvVarEntry {
	e := schemaEnvVarEntry{Path: path, Default: n["default"]}
	e.Type = n.typ()
	if items, ok := n["items"].(map[string]interface{}); ok {
		e.Items, _ = items["type"].(string)
	}
	if enum, ok := n["enum"].([]interface{}); ok {
		for _, v := range enum {
			e.Enum = append(e.Enum, fmt.Sprint(v))
		}
	}
	return e
}

// envContractKey is one setting the contract covers.
type envContractKey struct {
	Path   string
	Layer1 bool
	Node   schemaNode // nil when the schema has no node for the key
}

// envContractKeys returns the leaf registry KoanfPaths and the leaf keys
// under every Layer-0 prefix, sorted by path. A registry KoanfPath that is a
// parent of another registry KoanfPath (telemetry.cloud) is not a leaf.
func envContractKeys(t *testing.T, nodes map[string]schemaNode) []envContractKey {
	t.Helper()
	var registry []string
	for _, s := range Registry {
		registry = append(registry, s.KoanfPaths...)
	}
	isParent := func(p string, all []string) bool {
		for _, q := range all {
			if strings.HasPrefix(q, p+".") {
				return true
			}
		}
		return false
	}
	seen := map[string]bool{}
	var out []envContractKey
	add := func(p string, layer1 bool) {
		if seen[p] {
			return
		}
		seen[p] = true
		out = append(out, envContractKey{Path: p, Layer1: layer1, Node: nodes[p]})
	}
	for _, p := range registry {
		if !isParent(p, registry) {
			add(p, true)
		}
	}
	for _, prefix := range layer0Prefixes {
		n, ok := nodes[prefix]
		if !ok || !n.hasProperties() {
			add(prefix, false)
			continue
		}
		for p, child := range nodes {
			if strings.HasPrefix(p, prefix+".") && !child.hasProperties() {
				add(p, false)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// canonicalEnvSegments spells a dotted path as env segments: underscores
// inside a segment are removed, the segments are joined with "_".
func canonicalEnvSegments(path string) string {
	segs := strings.Split(path, ".")
	for i, s := range segs {
		segs[i] = strings.ToUpper(strings.ReplaceAll(s, "_", ""))
	}
	return strings.Join(segs, "_")
}

// derivedServerEnv is the SCION_SERVER_ spelling derived from a key's path:
// drop a leading "server.", then canonicalEnvSegments.
func derivedServerEnv(path string) string {
	return "SCION_SERVER_" + canonicalEnvSegments(strings.TrimPrefix(path, "server."))
}

// schemaEnvVarDerivationExceptions lists the settings-schema x-env-var names
// that derivedServerEnv does not produce, keyed by settings path, with the
// reason. TestSchemaEnvVarDerivation checks the list in both directions, so
// the derivation and the schema cannot drift apart unnoticed.
var schemaEnvVarDerivationExceptions = map[string]string{
	// SCION_SERVER_ names that differ from the derivation. These are the
	// canonical spelling of their key (canonicalServerEnv).
	"server.broker.allow_container_script_harnesses": envNameRuntimeBroker,
	"server.broker.container_hub_endpoint":           envNameRuntimeBroker,
	"server.broker.enabled":                          envNameRuntimeBroker,
	"server.broker.host":                             envNameRuntimeBroker,
	"server.broker.hub_endpoint":                     envNameRuntimeBroker,
	"server.broker.port":                             envNameRuntimeBroker,
	"server.broker.read_timeout":                     envNameRuntimeBroker,
	"server.broker.write_timeout":                    envNameRuntimeBroker,
	"server.broker.auto_provide":                     envNameUnderscored,
	"server.broker.broker_id":                        envNameUnderscored,
	"server.broker.broker_name":                      envNameUnderscored,
	"server.broker.broker_nickname":                  envNameUnderscored,
	"server.broker.broker_token":                     envNameUnderscored,
	"server.hub.public_url":                          "named after the GlobalConfig field hub.endpoint",

	// Names outside the SCION_SERVER_ namespace: the client settings
	// overlay (LoadVersionedSettings / LoadSettingsKoanf). No SCION_SERVER_
	// consumer reads them, so the contract keeps the derived SCION_SERVER_
	// spelling for these keys.
	"active_profile":                           envNameClient,
	"cli.autohelp":                             envNameClient,
	"cli.interactive_disabled":                 envNameClient,
	"default_harness_auth":                     envNameClient,
	"default_harness_config":                   envNameClient,
	"default_template":                         envNameClient,
	"hub.enabled":                              envNameClient,
	"hub.endpoint":                             envNameClient,
	"hub.local_only":                           envNameClient,
	"hub.project_id":                           envNameClient,
	"image_registry":                           envNameClient,
	"telemetry.cloud.cloud_logging":            envNameClient,
	"telemetry.cloud.enabled":                  envNameClient,
	"telemetry.cloud.endpoint":                 envNameClient,
	"telemetry.cloud.headers":                  envNameClient,
	"telemetry.cloud.protocol":                 envNameClient,
	"telemetry.cloud.tls.ca_file":              envNameClient,
	"telemetry.cloud.tls.insecure_skip_verify": envNameClient,
	"telemetry.enabled":                        envNameClient,
	"telemetry.hub.enabled":                    envNameClient,
}

const (
	envNameRuntimeBroker = "named after the GlobalConfig section runtimeBroker (SCION_SERVER_RUNTIMEBROKER_*)"
	envNameUnderscored   = "keeps the field's underscores, the spelling versionedEnvKeyMapper reads"
	envNameClient        = "client settings overlay name, outside SCION_SERVER_"
)

// canonicalServerEnv is the canonical SCION_SERVER_ spelling of a key: the
// schema's x-env-var name when it is a SCION_SERVER_ name, else the derived
// spelling.
func canonicalServerEnv(key envContractKey) string {
	if ev, _ := key.Node["x-env-var"].(string); strings.HasPrefix(ev, "SCION_SERVER_") {
		return ev
	}
	return derivedServerEnv(key.Path)
}

// canonicalSeedEnv is the canonical SCION_SEED_ spelling of a key: the
// canonical SCION_SERVER_ spelling with SCION_ replaced by SCION_SEED_ for
// server.* keys (SCION_SEED_SERVER_HUB_ADMINEMAILS), and SCION_SERVER_
// replaced by SCION_SEED_ for the rest (SCION_SEED_TELEMETRY_ENABLED).
func canonicalSeedEnv(key envContractKey) string {
	name := canonicalServerEnv(key)
	if strings.HasPrefix(key.Path, "server.") {
		return "SCION_SEED_" + strings.TrimPrefix(name, "SCION_")
	}
	return "SCION_SEED_" + strings.TrimPrefix(name, "SCION_SERVER_")
}

// TestSchemaEnvVarDerivation checks the derived SCION_SERVER_ spelling
// against every x-env-var name in the settings schema: each one either
// matches the derivation or is listed in schemaEnvVarDerivationExceptions,
// and each listed exception still differs from the derivation.
func TestSchemaEnvVarDerivation(t *testing.T) {
	nodes := collectSchemaNodes(t)
	count := 0
	for path, n := range nodes {
		ev, _ := n["x-env-var"].(string)
		if ev == "" {
			continue
		}
		count++
		_, listed := schemaEnvVarDerivationExceptions[path]
		derived := derivedServerEnv(path)
		switch {
		case ev != derived && !listed:
			t.Errorf("%s: x-env-var %s, derived spelling %s; rename one, or list %q in schemaEnvVarDerivationExceptions",
				path, ev, derived, path)
		case ev == derived && listed:
			t.Errorf("%s: x-env-var now matches the derived spelling; remove it from schemaEnvVarDerivationExceptions", path)
		}
	}
	for path := range schemaEnvVarDerivationExceptions {
		if ev, _ := nodes[path]["x-env-var"].(string); ev == "" {
			t.Errorf("schemaEnvVarDerivationExceptions entry %q has no x-env-var in the schema; remove it", path)
		}
	}
	if count < 50 {
		t.Fatalf("found only %d x-env-var names; the schema walker is broken", count)
	}
}

// globalConfigContractPath maps a koanf path to the GlobalConfig path
// (matched loosely by structLeaf): globalConfigPathFor, plus the flattened
// cors.* fields.
func globalConfigContractPath(path string) string {
	p := globalConfigPathFor(path)
	segs := strings.Split(p, ".")
	if n := len(segs); n >= 2 && segs[n-2] == "cors" {
		segs = append(segs[:n-2], "cors"+segs[n-1])
	}
	return strings.Join(segs, ".")
}

// structTypeHas reports whether t has a field at the dotted path, matching
// koanf tags as structLeaf does (ignoring case and underscores).
func structTypeHas(t reflect.Type, path string) bool {
	for _, seg := range strings.Split(path, ".") {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return false
		}
		found := false
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("koanf"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			if normKey(tag) == normKey(seg) {
				t = f.Type
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// consumerApplies reports whether the contract checks key through consumer.
func consumerApplies(consumer string, key envContractKey) bool {
	switch consumer {
	case consumerOpsettings:
		return true
	case consumerSeed:
		return key.Layer1
	case consumerGlobalConfig:
		return structTypeHas(reflect.TypeOf(config.GlobalConfig{}), globalConfigContractPath(key.Path))
	case consumerVersioned:
		return strings.HasPrefix(key.Path, "server.") &&
			structTypeHas(reflect.TypeOf(config.VersionedSettings{}), key.Path)
	}
	return false
}

// clearSettingsEnv unsets every SCION_SERVER_* and SCION_SEED_* variable
// for the rest of the test, so a value exported by the surrounding
// environment cannot mask or fake a result.
func clearSettingsEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "SCION_SERVER_") || strings.HasPrefix(name, "SCION_SEED_") {
			t.Setenv(name, "")
			_ = os.Unsetenv(name)
		}
	}
}

// envContractHarness holds the load paths and baselines one run uses.
type envContractHarness struct {
	configDir    string
	gcModes      []struct{ name, home string }
	gcBaseline   map[string]*config.GlobalConfig
	vsBaseline   *config.VersionedSettings
	versionsHome string
}

func newEnvContractHarness(t *testing.T) *envContractHarness {
	t.Helper()
	h := &envContractHarness{configDir: t.TempDir(), gcBaseline: map[string]*config.GlobalConfig{}}
	legacyHome := t.TempDir()
	settingsHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(settingsHome, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsHome, ".scion", "settings.yaml"),
		[]byte("schema_version: \"1\"\nserver:\n  mode: workstation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.gcModes = []struct{ name, home string }{{"legacy", legacyHome}, {"settings", settingsHome}}
	for _, m := range h.gcModes {
		t.Setenv("HOME", m.home)
		gc, err := config.LoadGlobalConfig(h.configDir)
		if err != nil {
			t.Fatalf("baseline LoadGlobalConfig (%s): %v", m.name, err)
		}
		h.gcBaseline[m.name] = gc
	}
	h.versionsHome = settingsHome
	t.Setenv("HOME", settingsHome)
	vs, err := config.LoadVersionedSettings("")
	if err != nil {
		t.Fatalf("baseline LoadVersionedSettings: %v", err)
	}
	h.vsBaseline = vs
	return h
}

// pickValue returns a sample value for key that differs from base (when
// base is valid), so a reached value is distinguishable from the default.
func pickValue(key envContractKey, base reflect.Value) string {
	var node schemaNode = key.Node
	if node == nil {
		node = schemaNode{"type": "string"}
	}
	// An empty sample (an enum may allow "") would read as unset.
	var samples []string
	for _, s := range sampleEnvValues(node.entry(key.Path)) {
		if s != "" {
			samples = append(samples, s)
		}
	}
	if len(samples) == 0 {
		return "envtest"
	}
	for _, s := range samples {
		if !base.IsValid() || !leafEquals(base, s) {
			return s
		}
	}
	return samples[0]
}

// koanfReach checks one env-only koanf loader: a set value lands on path
// verbatim, an empty one leaves path absent.
func koanfReach(t *testing.T, name, path, value string, load func() koanfGetter) (reached bool, emptyProblem string) {
	t.Helper()
	t.Setenv(name, value)
	k := load()
	reached = k.Exists(path) && fmt.Sprint(k.Get(path)) == value
	t.Setenv(name, "")
	if k := load(); k.Exists(path) {
		emptyProblem = fmt.Sprintf("%s=\"\" sets %s to %v; an empty value must be treated as unset", name, path, k.Get(path))
	}
	_ = os.Unsetenv(name)
	return reached, emptyProblem
}

// koanfGetter is the part of *koanf.Koanf koanfReach uses.
type koanfGetter interface {
	Exists(string) bool
	Get(string) interface{}
}

// leafString renders a struct leaf for messages and comparisons.
func leafString(v reflect.Value, ok bool) string {
	if !ok || !v.IsValid() {
		return "<absent>"
	}
	return fmt.Sprintf("%v", v.Interface())
}

// reachResult is the outcome of one (consumer, key) check.
type reachResult struct {
	reached bool
	detail  string // why it did not reach
	empty   []string
}

func (h *envContractHarness) check(t *testing.T, consumer string, key envContractKey) reachResult {
	t.Helper()
	var r reachResult
	switch consumer {
	case consumerOpsettings, consumerSeed:
		name, load := canonicalServerEnv(key), func() koanfGetter { return config.LoadEnvKoanf() }
		if consumer == consumerSeed {
			name, load = canonicalSeedEnv(key), func() koanfGetter { return config.LoadSeedEnvKoanf() }
		}
		v := pickValue(key, reflect.Value{})
		reached, empty := koanfReach(t, name, key.Path, v, load)
		r.reached = reached
		if !reached {
			r.detail = fmt.Sprintf("%s=%q does not land on %s", name, v, key.Path)
		}
		if empty != "" {
			r.empty = append(r.empty, empty)
		}

	case consumerGlobalConfig:
		name := canonicalServerEnv(key)
		gcPath := globalConfigContractPath(key.Path)
		r.reached = true
		for _, m := range h.gcModes {
			base, baseOK := structLeaf(reflect.ValueOf(h.gcBaseline[m.name]), gcPath)
			v := pickValue(key, base)
			t.Setenv("HOME", m.home)
			t.Setenv(name, v)
			gc, err := config.LoadGlobalConfig(h.configDir)
			if err != nil {
				r.reached = false
				r.detail += fmt.Sprintf("[%s] LoadGlobalConfig with %s=%q: %v; ", m.name, name, v, err)
			} else if got, ok := structLeaf(reflect.ValueOf(gc), gcPath); !ok || !leafEquals(got, v) {
				r.reached = false
				r.detail += fmt.Sprintf("[%s] %s=%q: GlobalConfig %s = %s; ", m.name, name, v, gcPath, leafString(got, ok))
			}
			t.Setenv(name, "")
			gc, err = config.LoadGlobalConfig(h.configDir)
			if err != nil {
				r.empty = append(r.empty, fmt.Sprintf("[%s] LoadGlobalConfig with %s=\"\": %v", m.name, name, err))
			} else if got, ok := structLeaf(reflect.ValueOf(gc), gcPath); leafString(got, ok) != leafString(base, baseOK) {
				r.empty = append(r.empty, fmt.Sprintf("[%s] %s=\"\" changes GlobalConfig %s from %s to %s",
					m.name, name, gcPath, leafString(base, baseOK), leafString(got, ok)))
			}
			_ = os.Unsetenv(name)
		}

	case consumerVersioned:
		name := canonicalServerEnv(key)
		t.Setenv("HOME", h.versionsHome)
		base, baseOK := structLeaf(reflect.ValueOf(h.vsBaseline), key.Path)
		v := pickValue(key, base)
		t.Setenv(name, v)
		vs, err := config.LoadVersionedSettings("")
		if err != nil {
			r.detail = fmt.Sprintf("LoadVersionedSettings with %s=%q: %v", name, v, err)
		} else if got, ok := structLeaf(reflect.ValueOf(vs), key.Path); !ok || !leafEquals(got, v) {
			r.detail = fmt.Sprintf("%s=%q: VersionedSettings %s = %s", name, v, key.Path, leafString(got, ok))
		} else {
			r.reached = true
		}
		t.Setenv(name, "")
		vs, err = config.LoadVersionedSettings("")
		if err != nil {
			r.empty = append(r.empty, fmt.Sprintf("LoadVersionedSettings with %s=\"\": %v", name, err))
		} else if got, ok := structLeaf(reflect.ValueOf(vs), key.Path); leafString(got, ok) != leafString(base, baseOK) {
			r.empty = append(r.empty, fmt.Sprintf("%s=\"\" changes VersionedSettings %s from %s to %s",
				name, key.Path, leafString(base, baseOK), leafString(got, ok)))
		}
		_ = os.Unsetenv(name)
	}
	return r
}

// TestServerEnvContract checks, for every registry KoanfPath and Layer-0
// key, that the canonical SCION_SERVER_ (and, for Layer-1 keys, SCION_SEED_)
// spelling reaches the key through every env consumer, and that an empty
// value is treated as unset (ptone/scion#3910). Known gaps are in
// envContractGaps.
func TestServerEnvContract(t *testing.T) {
	clearSettingsEnv(t)
	nodes := collectSchemaNodes(t)
	keys := envContractKeys(t, nodes)
	if len(keys) < 150 {
		t.Fatalf("found only %d contract keys; the key walker is broken", len(keys))
	}

	// The structured-key list must match the schema exactly.
	known := map[string]bool{}
	for _, k := range keys {
		known[k.Path] = true
		structured := k.Node != nil && k.Node.isStructured()
		if structured && !envContractStructuredKeys[k.Path] {
			t.Errorf("%s holds a map or a list of objects; add it to envContractStructuredKeys", k.Path)
		}
		if !structured && envContractStructuredKeys[k.Path] {
			t.Errorf("envContractStructuredKeys entry %q is not a map or a list of objects; remove it", k.Path)
		}
	}
	for p := range envContractStructuredKeys {
		if !known[p] {
			t.Errorf("envContractStructuredKeys entry %q is not a contract key; remove it", p)
		}
	}

	// Every gap entry must name a consumer that applies to a known key.
	byPath := map[string]envContractKey{}
	for _, k := range keys {
		byPath[k.Path] = k
	}
	for entry, reason := range envContractGaps {
		consumer, path, _ := strings.Cut(entry, " ")
		k, ok := byPath[path]
		if !ok || envContractStructuredKeys[path] || !consumerApplies(consumer, k) {
			t.Errorf("envContractGaps entry %q names no checked (consumer, key) pair; remove it", entry)
		}
		if !strings.Contains(reason, "ptone/scion#3911") {
			t.Errorf("envContractGaps entry %q: the reason must cite ptone/scion#3911", entry)
		}
	}

	h := newEnvContractHarness(t)
	for _, k := range keys {
		if envContractStructuredKeys[k.Path] {
			continue
		}
		t.Run(k.Path, func(t *testing.T) {
			for _, consumer := range envContractConsumers {
				if !consumerApplies(consumer, k) {
					continue
				}
				entry := consumer + " " + k.Path
				r := h.check(t, consumer, k)
				_, known := envContractGaps[entry]
				switch {
				case !r.reached && !known:
					t.Errorf("%s: canonical spelling does not reach the key through %s: %s; "+
						"map it, or add %q to envContractGaps", k.Path, consumer, r.detail, entry)
				case r.reached && known:
					t.Errorf("%s: the gap through %s is fixed; remove %q from envContractGaps",
						k.Path, consumer, entry)
				}
				for _, e := range r.empty {
					t.Errorf("%s (%s): %s", k.Path, consumer, e)
				}
			}
		})
	}
}
