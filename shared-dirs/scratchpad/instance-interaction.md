You are an AI agent whose primary role is to manage and interact with a GCP VM via `gcloud compute ssh --zone "us-central1-a" "scion-aiopm" --project "deploy-demo-test"`

If you do not have the ssh command already installed in your environment, you will need to install it with apt. You have sudo in this environment, and on the scion-aiopm GCE VM.

Note: this note was adapted and is re-used from an earlier project about building an A2A bridge - some leftover notes may still be in here and can be deleted for flagged for cleanup.

## VM Details

- **Instance**: `scion-aiopm`
- **Zone**: `us-central1-a`
- **Project**: `deploy-demo-test`
- **SSH user**: Logs in as a service account (`sa_*`), not as `scion`. Use `sudo -u scion bash -c '...'` to run commands as the scion user, or `sudo` for root-level operations.

## Repository Configuration

The scion repo is checked out at `/home/scion/scion` on the VM.

- **Remote**: `https://github.com/ptone/scion.git` (origin)
- **Branch**: `scion/a2a-bridge`
- **Purpose**: This VM is configured for integration testing of the `scion/a2a-bridge` branch. Changes are pushed from the development workspace to the remote, then pulled down onto the VM.

## Hub Service

- **Service**: `scion-hub` (systemd)
- **Config directory**: `/home/scion/.scion/`
- **Environment file**: `/home/scion/.scion/hub.env`
- **Settings**: `/home/scion/.scion/settings.yaml`
- **Database**: `/home/scion/.scion/hub.db`
- **Service file**: `/etc/systemd/system/scion-hub.service`
- **Binary**: `/usr/local/bin/scion`
- **Web UI / API port**: 8080 (behind Caddy reverse proxy)
- **Public URL**: `https://aiopm.projects.scion-ai.dev`
- **Caddy config**: `/etc/caddy/Caddyfile` (serves `aiopm.projects.scion-ai.dev`)

### Key hub.env settings
- `SCION_MAINTENANCE_REPO_PATH="/home/scion/scion"` — points rebuild operations at the local checkout
- `SCION_MAINTENANCE_REPO_BRANCH=scion/chat-tee` — pins rebuilds to this branch

## Common Operations

### Check service status
```bash
gcloud compute ssh --zone "us-central1-a" "scion-aiopm" --project "deploy-demo-test" --command "sudo systemctl status scion-hub"
```

### Pull latest code on VM
```bash
gcloud compute ssh --zone "us-central1-a" "scion-aiopm" --project "deploy-demo-test" --command "sudo -u scion bash -c 'cd /home/scion/scion && git pull origin scion/chat-tee'"
```

### Rebuild and restart hub
```bash
gcloud compute ssh --zone "us-central1-a" "scion-aiopm" --project "deploy-demo-test" --command "
sudo -u scion bash -c 'cd /home/scion/scion && git pull origin scion/a2a-bridge && make web && /usr/local/go/bin/go build -o scion ./cmd/scion'
sudo systemctl stop scion-hub
sudo mv /home/scion/scion/scion /usr/local/bin/scion
sudo chmod +x /usr/local/bin/scion
sudo systemctl start scion-hub
"
```

### View recent logs
```bash
gcloud compute ssh --zone "us-central1-a" "scion-aiopm" --project "deploy-demo-test" --command "sudo journalctl -u scion-hub -n 50 --no-pager"
```

### Health check
```bash
gcloud compute ssh --zone "us-central1-a" "scion-aiopm" --project "deploy-demo-test" --command "curl -s http://localhost:8080/healthz"
```

## Integration Testing Workflow

1. Make changes in the development workspace on branch `scion/a2a-bridge`
2. Push to remote: `git push origin scion/a2a-bridge`
3. Pull on VM and rebuild (see commands above), or trigger a rebuild via the hub's admin maintenance UI
4. Test against `https://integration.projects.scion-ai.dev`


## SSH Notes

- **Do NOT use `--tunnel-through-iap`** — the VM has an external IP (35.232.118.211) and OS Login. Direct SSH works fine.
- The previous instance `scion-integration` is not in use — always use `scion-aiopm`
- `integration.projects.scion-ai.dev` (136.111.240.153) is the OLD VM — do not use
- `aiopm.projects.scion-ai.dev` (35.232.118.211) is THIS VM
- The hub can also self-rebuild via its admin maintenance page (rebuild-server / rebuild-web tasks), which respect the `SCION_MAINTENANCE_REPO_BRANCH` setting


