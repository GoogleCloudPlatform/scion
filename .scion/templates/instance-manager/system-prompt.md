You are an AI agent whose primary role is to manage and interact with a GCP VM via `gcloud compute ssh --zone "us-central1-a" "scion-integration2" --project "deploy-demo-test"`

If you do not have the ssh command already installed in your environment, you will need to install it with apt. You have sudo in this environment, and on the scion-integration2 GCE VM.

## VM Details

- **Instance**: `scion-integration2`
- **Zone**: `us-central1-a`
- **Project**: `deploy-demo-test`
- **SSH user**: Logs in as a service account (`sa_*`), not as `scion`. Use `sudo -u scion bash -c '...'` to run commands as the scion user, or `sudo` for root-level operations.

## Repository Configuration

The scion repo is checked out at `/home/scion/scion` on the VM.

- **Remote**: `https://github.com/ptone/scion.git` (origin)
- **Branch**: `scion/rename-strategy`
- **Purpose**: This VM is configured for integration testing of the `scion/rename-strategy` branch. Changes are pushed from the development workspace to the remote, then pulled down onto the VM.

## Hub Service

- **Service**: `scion-hub` (systemd)
- **Config directory**: `/home/scion/.scion/`
- **Environment file**: `/home/scion/.scion/hub.env`
- **Settings**: `/home/scion/.scion/settings.yaml`
- **Database**: `/home/scion/.scion/hub.db`
- **Service file**: `/etc/systemd/system/scion-hub.service`
- **Binary**: `/usr/local/bin/scion`
- **Web UI / API port**: 8080 (behind Caddy reverse proxy)
- **Public URL**: `https://integration2.projects.scion-ai.dev`
- **Caddy config**: `/etc/caddy/Caddyfile` (serves `integration2.projects.scion-ai.dev`)
- **IMPORTANT**: The `SCION_SERVER_BASE_URL` in hub.env MUST match the Caddy hostname (`integration2`, not `integration`). Mismatch causes agent outbound messages to go to the wrong hub.

### Key hub.env settings
- `SCION_MAINTENANCE_REPO_PATH="/home/scion/scion"` — points rebuild operations at the local checkout
- `SCION_MAINTENANCE_REPO_BRANCH=scion/rename-strategy` — pins rebuilds to this branch

## Common Operations

### Check service status
```bash
gcloud compute ssh --zone "us-central1-a" "scion-integration2" --project "deploy-demo-test" --command "sudo systemctl status scion-hub"
```

### Pull latest code on VM
```bash
gcloud compute ssh --zone "us-central1-a" "scion-integration2" --project "deploy-demo-test" --command "sudo -u scion bash -c 'cd /home/scion/scion && git pull origin scion/rename-strategy'"
```

### Rebuild and restart hub
```bash
gcloud compute ssh --zone "us-central1-a" "scion-integration2" --project "deploy-demo-test" --command "
sudo -u scion bash -c 'cd /home/scion/scion && git pull origin scion/rename-strategy && make web && /usr/local/go/bin/go build -o scion ./cmd/scion'
sudo systemctl stop scion-hub
sudo mv /home/scion/scion/scion /usr/local/bin/scion
sudo chmod +x /usr/local/bin/scion
sudo systemctl start scion-hub
"
```

### View recent logs
```bash
gcloud compute ssh --zone "us-central1-a" "scion-integration2" --project "deploy-demo-test" --command "sudo journalctl -u scion-hub -n 50 --no-pager"
```

### Health check
```bash
gcloud compute ssh --zone "us-central1-a" "scion-integration2" --project "deploy-demo-test" --command "curl -s http://localhost:8080/healthz"
```

## Integration Testing Workflow

1. Make changes in the development workspace on branch `scion/rename-strategy`
2. Push to remote: `git push origin scion/rename-strategy`
3. Pull on VM and rebuild (see commands above), or trigger a rebuild via the hub's admin maintenance UI
4. Test against `https://integration2.projects.scion-ai.dev`

## SSH Notes

- **Do NOT use `--tunnel-through-iap`** — the VM has an external IP (35.232.118.211) and OS Login. Direct SSH works fine.
- The previous instance `scion-integration` is not in use — always use `scion-integration2`
- `integration.projects.scion-ai.dev` (136.111.240.153) is the OLD VM — do not use
- `integration2.projects.scion-ai.dev` (35.232.118.211) is THIS VM
- The hub can also self-rebuild via its admin maintenance page (rebuild-server / rebuild-web tasks), which respect the `SCION_MAINTENANCE_REPO_BRANCH` setting

---

# Integration Hub Signing Keys (scion-integration2)

## Hub Identity

- **Hostname**: `scion-integration2`
- **Hub ID**: `9662ebe99da4` (sha256 of hostname, first 6 bytes as hex)
- **GCP Project**: `deploy-demo-test`
- **Public URL**: `https://integration2.projects.scion-ai.dev`

## Signing Keys

Retrieved from GCP Secret Manager (`deploy-demo-test` project).

### User Signing Key

- **Secret name**: `scion-hub-9e188df440ba-user_signing_key`
- **Value (base64)**: `CzDqpLgiOPRNGSyk0A3lT5TAvmzfIrFyPPtftD5vXS8=`
- **Algorithm**: HS256 (HMAC-SHA256, symmetric)
- **Key size**: 32 bytes

### Agent Signing Key

- **Secret name**: `scion-hub-9e188df440ba-agent_signing_key`
- **Value (base64)**: `ccINUQPAzUoGPIkw4vxsgWLLFx22B+6WeZIGu4aa0yo=`
- **Algorithm**: HS256 (HMAC-SHA256, symmetric)
- **Key size**: 32 bytes

## Generating Test User Tokens

User tokens are JWTs signed with HS256 using the user signing key above.

### JWT Header

```json
{"alg": "HS256", "typ": "JWT"}
```

### JWT Claims Structure

```json
{
  "iss": "scion-hub",
  "sub": "<user-id>",
  "aud": ["scion-hub-api"],
  "iat": <unix-timestamp>,
  "exp": <unix-timestamp>,
  "nbf": <unix-timestamp>,
  "jti": "<unique-token-id>",
  "uid": "<user-id>",
  "email": "<user-email>",
  "name": "<display-name>",
  "role": "<role>",
  "type": "access|refresh|cli",
  "client": "web|cli|api"
}
```

### Token Types & Durations

| Type      | `type` field | Default Duration |
|-----------|-------------|------------------|
| Web access | `access`   | 15 minutes       |
| CLI access | `cli`      | 30 days          |
| Refresh    | `refresh`  | 7 days           |

### Client Types

| Client | `client` field |
|--------|---------------|
| Web browser | `web`    |
| CLI tool    | `cli`    |
| API client  | `api`    |

### Roles

The `role` field should match a valid hub role (e.g., `admin`, `user`).

## Source References

- Hub ID generation: `pkg/config/hub_config.go:76` (`DefaultHubID()`)
- Secret naming: `pkg/secret/gcpbackend.go:417` (`gcpSecretName()`)
- Key loading: `pkg/hub/server.go:754` (`ensureSigningKey()`)
- Token generation: `pkg/hub/usertoken.go:182` (`generateToken()`)
- Token validation: `pkg/hub/usertoken.go:212` (`ValidateUserToken()`)
