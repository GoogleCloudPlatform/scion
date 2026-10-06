
### Schema Sketch (v1)

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://scion.dev/schemas/settings/v1.json",
  "title": "Scion Settings",
  "description": "Configuration for the Scion agent orchestration platform (v1).",
  "type": "object",
  "properties": {
    "$schema": {
      "type": "string",
      "description": "JSON Schema URI for IDE support."
    },
    "schema_version": {
      "type": "string",
      "const": "1",
      "description": "Settings schema version. Required for versioned settings.",
      "x-since": "1"
    },
    "active_profile": {
      "type": "string",
      "default": "local",
      "description": "Name of the active profile.",
      "x-env-var": "SCION_ACTIVE_PROFILE",
      "x-scope": "any",
      "x-since": "1"
    },
    "default_template": {
      "type": "string",
      "description": "Default template for new agents. Preserved for backward compatibility; prefer setting this per-profile.",
      "x-env-var": "SCION_DEFAULT_TEMPLATE",
      "x-scope": "any",
      "x-since": "1"
    },
    "server": {
      "type": "object",
      "description": "Server/broker process configuration. Global-only.",
      "x-scope": "global",
      "x-since": "1",
      "properties": {
        "env": {
          "type": "string",
          "description": "Deployment environment label (e.g., dev, staging, prod).",
          "x-env-var": "SCION_SERVER_ENV",
          "x-since": "1"
        },
        "hub": { "$ref": "#/$defs/serverHub" },
        "broker": { "$ref": "#/$defs/serverBroker" },
        "database": { "$ref": "#/$defs/serverDatabase" },
        "auth": { "$ref": "#/$defs/serverAuth" },
        "oauth": { "$ref": "#/$defs/serverOAuth" },
        "storage": { "$ref": "#/$defs/serverStorage" },
        "secrets": { "$ref": "#/$defs/serverSecrets" },
        "log_level": {
          "type": "string",
          "enum": ["debug", "info", "warn", "error"],
          "default": "info",
          "x-env-var": "SCION_SERVER_LOG_LEVEL",
          "x-since": "1"
        },
        "log_format": {
          "type": "string",
          "enum": ["text", "json"],
          "default": "text",
          "x-env-var": "SCION_SERVER_LOG_FORMAT",
          "x-since": "1"
        }
      },
      "additionalProperties": false
    },
    "hub": {
      "type": "object",
      "description": "Hub client connection settings. Auth uses server.auth.dev_token (dev mode) or OAuth (production).",
      "x-scope": "any",
      "x-since": "1",
      "properties": {
        "enabled": {
          "type": "boolean",
          "description": "Enable Hub integration.",
          "x-env-var": "SCION_HUB_ENABLED",
          "x-since": "1"
        },
        "endpoint": {
          "type": "string",
          "format": "uri",
          "description": "Hub API endpoint URL to connect to as a client.",
          "x-env-var": "SCION_HUB_ENDPOINT",
          "x-since": "1"
        },
        "grove_id": {
          "type": "string",
          "description": "Grove identifier when registered with the Hub.",
          "x-env-var": "SCION_HUB_GROVE_ID",
          "x-since": "1"
        },
        "local_only": {
          "type": "boolean",
          "description": "Operate in local-only mode even when Hub is configured. Hub sync checks will error with guidance to use --no-hub.",
          "x-env-var": "SCION_HUB_LOCAL_ONLY",
          "x-since": "1"
        }
      },
      "additionalProperties": false
    },
    "cli": {
      "type": "object",
      "description": "CLI behavior settings.",
      "x-scope": "any",
      "x-since": "1",
      "properties": {
        "autohelp": {
          "type": "boolean",
          "default": true,
          "description": "Print the usage block after an argument or flag error.",
          "x-env-var": "SCION_CLI_AUTOHELP",
          "x-since": "1"
        },
        "interactive_disabled": {
          "type": "boolean",
          "default": false,
          "description": "Disable interactive prompts (useful for CI/scripts).",
          "x-env-var": "SCION_CLI_INTERACTIVE_DISABLED",
          "x-since": "1"
        }
      },
      "additionalProperties": false
    },
    "runtimes": {
      "type": "object",
      "description": "Named container runtime definitions. Map keys are arbitrary labels; the 'type' field determines the runtime type.",
      "x-scope": "any",
      "x-since": "1",
      "additionalProperties": {
        "$ref": "#/$defs/runtimeConfig"
      }
    },
    "harness_configs": {
      "type": "object",
      "description": "Named harness configurations. Multiple configs may share a harness type.",
      "x-scope": "any",
      "x-since": "1",
      "additionalProperties": {
        "$ref": "#/$defs/harnessConfig"
      }
    },
    "profiles": {
      "type": "object",
      "description": "Named environment profiles.",
      "x-scope": "any",
      "x-since": "1",
      "additionalProperties": {
        "$ref": "#/$defs/profileConfig"
      }
    }
  },
  "additionalProperties": false,
  "$defs": {
    "runtimeConfig": {
      "type": "object",
      "properties": {
        "type": {
          "type": "string",
          "enum": ["docker", "container", "kubernetes"],
          "description": "Runtime type. Defaults to the runtime entry name if omitted."
        },
        "host": { "type": "string" },
        "context": { "type": "string" },
        "namespace": { "type": "string" },
        "tmux": { "type": "boolean" },
        "env": {
          "type": "object",
          "additionalProperties": { "type": "string" }
        },
        "sync": { "type": "string" }
      },
      "additionalProperties": false
    },
    "harnessConfig": {
      "type": "object",
      "required": ["harness"],
      "properties": {
        "harness": {
          "type": "string",
          "enum": ["gemini", "claude", "opencode", "codex", "generic"],
          "description": "The harness type this config applies to."
        },
        "image": {
          "type": "string",
          "description": "Container image URI."
        },
        "user": {
          "type": "string",
          "description": "Unix user inside the container."
        },
        "model": {
          "type": "string",
          "description": "LLM model identifier (new; not in current HarnessConfig)."
        },
        "args": {
          "type": "array",
          "items": { "type": "string" },
          "description": "Additional harness CLI arguments (new; not in current HarnessConfig)."
        },
        "env": {
          "type": "object",
          "additionalProperties": { "type": "string" }
        },
        "volumes": {
          "type": "array",
          "items": { "$ref": "#/$defs/volumeMount" }
        },
        "auth_selected_type": {
          "type": "string",
          "description": "Authentication mechanism to use (e.g., gemini-api-key, vertex-ai, oauth-personal)."
        }
      },
      "additionalProperties": false
    },
    "profileConfig": {
      "type": "object",
      "required": ["runtime"],
      "properties": {
        "runtime": {
          "type": "string",
          "description": "Name of the runtime (key in runtimes map) to use."
        },
        "default_template": {
          "type": "string",
          "description": "Default template for agents created under this profile."
        },
        "default_harness_config": {
          "type": "string",
          "description": "Default harness config name for agents under this profile."
        },
        "tmux": { "type": "boolean" },
        "env": {
          "type": "object",
          "additionalProperties": { "type": "string" }
        },
        "volumes": {
          "type": "array",
          "items": { "$ref": "#/$defs/volumeMount" }
        },
        "resources": { "$ref": "#/$defs/resourceSpec" },
        "harness_overrides": {
          "type": "object",
          "description": "Per-harness-config overrides applied when using this profile. Keys are harness-config names (not harness types).",
          "additionalProperties": { "$ref": "#/$defs/harnessOverride" }
        }
      },
      "additionalProperties": false
    },
    "harnessOverride": {
      "type": "object",
      "properties": {
        "image": { "type": "string" },
        "user": { "type": "string" },
        "env": {
          "type": "object",
          "additionalProperties": { "type": "string" }
        },
        "volumes": {
          "type": "array",
          "items": { "$ref": "#/$defs/volumeMount" }
        },
        "resources": { "$ref": "#/$defs/resourceSpec" },
        "auth_selected_type": { "type": "string" }
      },
      "additionalProperties": false
    },
    "volumeMount": {
      "type": "object",
      "required": ["target"],
      "properties": {
        "source": { "type": "string" },
        "target": { "type": "string" },
        "read_only": { "type": "boolean", "default": false },
        "type": { "type": "string", "enum": ["local", "gcs"], "default": "local" },
        "bucket": { "type": "string" },
        "prefix": { "type": "string" },
        "mode": { "type": "string" }
      }
    },
    "resourceSpec": {
      "type": "object",
      "properties": {
        "requests": {
          "type": "object",
          "properties": {
            "cpu": { "type": "string" },
            "memory": { "type": "string" }
          }
        },
        "limits": {
          "type": "object",
          "properties": {
            "cpu": { "type": "string" },
            "memory": { "type": "string" }
          }
        },
        "disk": { "type": "string" }
      }
    },
    "serverHub": {
      "type": "object",
      "description": "Hub API server settings (for running scion-server).",
      "properties": {
        "port": { "type": "integer", "default": 9810, "x-env-var": "SCION_SERVER_HUB_PORT" },
        "host": { "type": "string", "default": "0.0.0.0", "x-env-var": "SCION_SERVER_HUB_HOST" },
        "public_url": {
          "type": "string",
          "format": "uri",
          "description": "Public-facing URL for this Hub server. Passed to agents for status callbacks. Not the same as hub.endpoint (client-side). Note: the purpose of this field may need further investigation and cleanup.",
          "x-env-var": "SCION_SERVER_HUB_ENDPOINT",
          "x-env-var-alias": "SCION_SERVER_HUB_PUBLIC_URL"
        },
        "read_timeout": { "type": "string", "default": "30s", "x-env-var": "SCION_SERVER_HUB_READTIMEOUT" },
        "write_timeout": { "type": "string", "default": "60s", "x-env-var": "SCION_SERVER_HUB_WRITETIMEOUT" },
        "cors": { "$ref": "#/$defs/corsConfig" },
        "admin_emails": {
          "type": "array",
          "items": { "type": "string", "format": "email" },
          "description": "Email addresses to auto-promote to admin role.",
          "x-env-var": "SCION_SERVER_HUB_ADMINEMAIL"
        }
      }
    },
    "serverBroker": {
      "type": "object",
      "description": "Broker API server and identity settings. Renamed from serverRuntimeBroker.",
      "properties": {
        "enabled": { "type": "boolean", "default": false, "x-env-var": "SCION_SERVER_BROKER_ENABLED" },
        "port": { "type": "integer", "default": 9800, "x-env-var": "SCION_SERVER_BROKER_PORT" },
        "host": { "type": "string", "default": "0.0.0.0", "x-env-var": "SCION_SERVER_BROKER_HOST" },
        "read_timeout": { "type": "string", "default": "30s", "x-env-var": "SCION_SERVER_BROKER_READTIMEOUT" },
        "write_timeout": { "type": "string", "default": "120s", "x-env-var": "SCION_SERVER_BROKER_WRITETIMEOUT" },
        "hub_endpoint": {
          "type": "string",
          "format": "uri",
          "description": "Hub API endpoint for this broker to report status to.",
          "x-env-var": "SCION_SERVER_BROKER_HUBENDPOINT"
        },
        "container_hub_endpoint": {
          "type": "string",
          "format": "uri",
          "description": "Overrides hub_endpoint when injecting the Hub URL into agent containers. Use when containers cannot reach the Hub at the broker's address (e.g. http://host.containers.internal:8080 for local development).",
          "x-env-var": "SCION_SERVER_BROKER_CONTAINERHUBENDPOINT"
        },
        "broker_id": {
          "type": "string",
          "description": "Unique broker identifier (UUID). Auto-generated if empty.",
          "x-env-var": "SCION_SERVER_BROKER_BROKERID"
        },
        "broker_name": {
          "type": "string",
          "description": "Human-readable broker name.",
          "x-env-var": "SCION_SERVER_BROKER_BROKERNAME"
        },
        "broker_nickname": {
          "type": "string",
          "description": "Human-readable display name for the broker. Defaults to hostname.",
          "x-env-var": "SCION_SERVER_BROKER_BROKERNICKNAME"
        },
        "broker_token": {
          "type": "string",
          "description": "Token received when registering this broker with the Hub.",
          "x-sensitive": true,
          "x-env-var": "SCION_SERVER_BROKER_BROKERTOKEN"
        },
        "cors": { "$ref": "#/$defs/corsConfig" }
      }
    },
    "corsConfig": {
      "type": "object",
      "properties": {
        "enabled": { "type": "boolean", "default": true },
        "allowed_origins": {
          "type": "array",
          "items": { "type": "string" },
          "default": ["*"]
        },
        "allowed_methods": {
          "type": "array",
          "items": { "type": "string" }
        },
        "allowed_headers": {
          "type": "array",
          "items": { "type": "string" }
        },
        "max_age": { "type": "integer", "default": 3600 }
      }
    },
    "serverDatabase": {
      "type": "object",
      "properties": {
        "driver": { "type": "string", "enum": ["sqlite", "postgres"], "default": "sqlite", "x-env-var": "SCION_SERVER_DATABASE_DRIVER" },
        "url": { "type": "string", "x-env-var": "SCION_SERVER_DATABASE_URL" }
      }
    },
    "serverAuth": {
      "type": "object",
      "description": "Auth settings. dev_token/dev_token_file are used by both the server (to accept) and the hub client (to send) in dev mode.",
      "properties": {
        "dev_mode": { "type": "boolean", "default": false, "x-env-var": "SCION_SERVER_AUTH_DEVMODE" },
        "dev_token": {
          "type": "string",
          "description": "Dev auth token. Used by the server to accept dev requests and by the hub client to authenticate in dev mode. Also configurable via SCION_DEV_TOKEN env var or ~/.scion/dev-token file.",
          "x-sensitive": true,
          "x-env-var": "SCION_SERVER_AUTH_DEVTOKEN"
        },
        "dev_token_file": { "type": "string", "x-env-var": "SCION_SERVER_AUTH_DEVTOKENFILE" },
        "authorized_domains": {
          "type": "array",
          "items": { "type": "string" },
          "x-env-var": "SCION_SERVER_AUTH_AUTHORIZEDDOMAINS"
        }
      }
    },
    "serverOAuth": {
      "type": "object",
      "description": "OAuth provider configurations. Web, CLI, and Device use separate OAuth clients due to different redirect URI requirements.",
      "properties": {
        "web": { "$ref": "#/$defs/oauthClientConfig" },
        "cli": { "$ref": "#/$defs/oauthClientConfig" },
        "device": { "$ref": "#/$defs/oauthClientConfig" }
      }
    },
    "oauthClientConfig": {
      "type": "object",
      "properties": {
        "google": { "$ref": "#/$defs/oauthProviderConfig" },
        "github": { "$ref": "#/$defs/oauthProviderConfig" }
      }
    },
    "oauthProviderConfig": {
      "type": "object",
      "properties": {
        "client_id": { "type": "string" },
        "client_secret": { "type": "string", "x-sensitive": true }
      }
    },
    "serverStorage": {
      "type": "object",
      "description": "Unified storage configuration. In the target state, a single bucket is used for all GCS storage (templates, workspaces, volumes) with path-based namespacing. See Phase 7 in the design doc.",
      "properties": {
        "provider": { "type": "string", "enum": ["local", "gcs"], "default": "local", "x-env-var": "SCION_SERVER_STORAGE_PROVIDER" },
        "bucket": { "type": "string", "description": "GCS bucket name. All storage types are namespaced under path prefixes within this bucket.", "x-env-var": "SCION_SERVER_STORAGE_BUCKET" },
        "local_path": { "type": "string", "x-env-var": "SCION_SERVER_STORAGE_LOCALPATH" }
      }
    },
    "serverSecrets": {
      "type": "object",
      "properties": {
        "backend": { "type": "string", "enum": ["local", "gcpsm"], "default": "local", "x-env-var": "SCION_SERVER_SECRETS_BACKEND" },
        "gcp_project_id": { "type": "string", "x-env-var": "SCION_SERVER_SECRETS_GCPPROJECTID" },
        "gcp_credentials": { "type": "string", "x-env-var": "SCION_SERVER_SECRETS_GCPCREDENTIALS" }
      }
    }
  }
}
```
