#!/usr/bin/env bash
# Deploy Scion hub as a Cloud Run service with co-located GKE broker.
#
# Prerequisites:
#   - gcloud CLI authenticated with sufficient permissions
#   - docker CLI authenticated to Artifact Registry
#   - kubectl configured for scion-demo-cluster (for namespace setup only)
#
# Usage:
#   ./scripts/cloudrun/deploy.sh          # full deploy (build + push + secrets + service)
#   ./scripts/cloudrun/deploy.sh --skip-build   # redeploy without rebuilding image

set -euo pipefail

# ── Configuration ────────────────────────────────────────────────────────────

PROJECT="${SCION_PROJECT:-deploy-demo-test}"
REGION="${SCION_REGION:-us-central1}"
SERVICE_NAME="${SCION_SERVICE:-scion-hub}"
GKE_CLUSTER="${SCION_GKE_CLUSTER:-scion-demo-cluster}"
SA_NAME="${SCION_SA_NAME:-scion-hub-sa}"
REPO="${SCION_REPO:-scion}"
IMAGE="us-central1-docker.pkg.dev/${PROJECT}/${REPO}/hub:latest"
K8S_NAMESPACE="scion-agents"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

SKIP_BUILD=false
[[ "${1:-}" == "--skip-build" ]] && SKIP_BUILD=true

# ── Helpers ──────────────────────────────────────────────────────────────────

log() { echo "==> $*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

ensure_secret() {
  local name="$1"
  local data="$2"
  if gcloud secrets describe "$name" --project="$PROJECT" &>/dev/null; then
    log "Updating secret ${name}"
    echo "$data" | gcloud secrets versions add "$name" --data-file=- --project="$PROJECT"
  else
    log "Creating secret ${name}"
    echo "$data" | gcloud secrets create "$name" --data-file=- --project="$PROJECT" \
      --replication-policy=automatic
  fi
}

# ── 0. Validate ──────────────────────────────────────────────────────────────

command -v gcloud >/dev/null || die "gcloud CLI not found"
command -v docker >/dev/null || die "docker CLI not found"

# ── 1. Service account ──────────────────────────────────────────────────────

SA_EMAIL="${SA_NAME}@${PROJECT}.iam.gserviceaccount.com"

if ! gcloud iam service-accounts describe "$SA_EMAIL" --project="$PROJECT" &>/dev/null; then
  log "Creating service account ${SA_NAME}"
  gcloud iam service-accounts create "$SA_NAME" \
    --display-name="Scion Hub (Cloud Run)" \
    --project="$PROJECT"

  for role in roles/container.admin roles/secretmanager.secretAccessor; do
    gcloud projects add-iam-policy-binding "$PROJECT" \
      --member="serviceAccount:${SA_EMAIL}" \
      --role="$role" \
      --condition=None \
      --quiet
  done
fi

# ── 2. Build & push image ───────────────────────────────────────────────────

if [[ "$SKIP_BUILD" == false ]]; then
  log "Building container image"
  docker build -f "${SCRIPT_DIR}/Dockerfile" -t "$IMAGE" "$REPO_ROOT"

  log "Pushing image to Artifact Registry"
  docker push "$IMAGE"
else
  log "Skipping build (--skip-build)"
fi

# ── 3. Generate kubeconfig from live cluster info ────────────────────────────

log "Fetching GKE cluster details"
ENDPOINT=$(gcloud container clusters describe "$GKE_CLUSTER" \
  --region "$REGION" --project "$PROJECT" \
  --format="value(endpoint)")
CA_CERT=$(gcloud container clusters describe "$GKE_CLUSTER" \
  --region "$REGION" --project "$PROJECT" \
  --format="value(masterAuth.clusterCaCertificate)")

[[ -n "$ENDPOINT" ]] || die "Could not fetch cluster endpoint"
[[ -n "$CA_CERT"  ]] || die "Could not fetch cluster CA certificate"

KUBECONFIG_CONTENT="apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: ${CA_CERT}
    server: https://${ENDPOINT}
  name: ${GKE_CLUSTER}
contexts:
- context:
    cluster: ${GKE_CLUSTER}
    namespace: ${K8S_NAMESPACE}
  name: ${GKE_CLUSTER}
current-context: ${GKE_CLUSTER}"

# ── 4. Generate hub settings ────────────────────────────────────────────────

SESSION_SECRET="${SCION_SESSION_SECRET:-$(openssl rand -hex 32)}"

SETTINGS_CONTENT=$(sed "s/__SESSION_SECRET__/${SESSION_SECRET}/" \
  "${SCRIPT_DIR}/hub-settings-template.yaml")

# ── 5. Store secrets ────────────────────────────────────────────────────────

log "Storing secrets in Secret Manager"
ensure_secret "${SERVICE_NAME}-kubeconfig" "$KUBECONFIG_CONTENT"
ensure_secret "${SERVICE_NAME}-settings"   "$SETTINGS_CONTENT"

# ── 6. Ensure K8s namespace ─────────────────────────────────────────────────

log "Ensuring namespace ${K8S_NAMESPACE} exists in ${GKE_CLUSTER}"
kubectl create namespace "$K8S_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - || true

# ── 7. Create Artifact Registry repo (if needed) ────────────────────────────

if ! gcloud artifacts repositories describe "$REPO" \
  --location="$REGION" --project="$PROJECT" &>/dev/null; then
  log "Creating Artifact Registry repository ${REPO}"
  gcloud artifacts repositories create "$REPO" \
    --repository-format=docker \
    --location="$REGION" \
    --project="$PROJECT"
fi

# ── 8. Deploy Cloud Run service ─────────────────────────────────────────────

log "Deploying Cloud Run service ${SERVICE_NAME}"
gcloud run deploy "$SERVICE_NAME" \
  --image "$IMAGE" \
  --region "$REGION" \
  --project "$PROJECT" \
  --min-instances 1 \
  --max-instances 1 \
  --no-allow-unauthenticated \
  --service-account "$SA_EMAIL" \
  --port 8080 \
  --memory 1Gi \
  --cpu 1 \
  --timeout 3600 \
  --set-secrets "/home/scion/.kube/config=${SERVICE_NAME}-kubeconfig:latest,/run/secrets/settings.yaml=${SERVICE_NAME}-settings:latest" \
  --set-env-vars "HOME=/home/scion,KUBECONFIG=/home/scion/.kube/config"

# ── 9. Print service URL ────────────────────────────────────────────────────

SERVICE_URL=$(gcloud run services describe "$SERVICE_NAME" \
  --region "$REGION" --project "$PROJECT" \
  --format="value(status.url)")

log "Deployment complete"
echo ""
echo "  Service URL: ${SERVICE_URL}"
echo "  Health check: curl -H \"Authorization: Bearer \$(gcloud auth print-identity-token)\" ${SERVICE_URL}/healthz"
echo ""
