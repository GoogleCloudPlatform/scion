#!/usr/bin/env bash
# Deploy Scion hub as a Cloud Run service with IAP enabled directly.
#
# Architecture:
#   User → Cloud Run (with built-in IAP) → Hub container
#
# Cloud Run's native IAP integration protects all ingress paths (including
# the default *.run.app URL) without requiring a load balancer, NEG, or
# managed certificate.
#
# Prerequisites:
#   - gcloud CLI authenticated with sufficient permissions
#   - docker CLI authenticated to Artifact Registry
#   - kubectl configured for scion-demo-cluster (for namespace setup only)
#   - IAP API enabled in the project (gcloud services enable iap.googleapis.com)
#
# Usage:
#   ./scripts/cloudrun/deploy.sh                # full deploy
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

# Optional: custom OAuth client for IAP (needed for external users)
IAP_CLIENT_ID="${SCION_IAP_CLIENT_ID:-}"
IAP_CLIENT_SECRET="${SCION_IAP_CLIENT_SECRET:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

SKIP_BUILD=false
for arg in "$@"; do
  case "$arg" in
    --skip-build) SKIP_BUILD=true ;;
  esac
done

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

# ── 1. Service account (hub) ────────────────────────────────────────────────

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

# ── 1b. Transport service account (for agent → hub IAP traversal) ───────────

TRANSPORT_SA_NAME="${SA_NAME}-transport"
TRANSPORT_SA_EMAIL="${TRANSPORT_SA_NAME}@${PROJECT}.iam.gserviceaccount.com"

if ! gcloud iam service-accounts describe "$TRANSPORT_SA_EMAIL" --project="$PROJECT" &>/dev/null; then
  log "Creating transport service account ${TRANSPORT_SA_NAME}"
  gcloud iam service-accounts create "$TRANSPORT_SA_NAME" \
    --display-name="Scion Transport (IAP traversal)" \
    --project="$PROJECT"

  # Hub SA needs to mint tokens as the transport SA
  gcloud iam service-accounts add-iam-policy-binding "$TRANSPORT_SA_EMAIL" \
    --member="serviceAccount:${SA_EMAIL}" \
    --role="roles/iam.serviceAccountTokenCreator" \
    --project="$PROJECT" \
    --quiet
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
read -r ENDPOINT CA_CERT < <(gcloud container clusters describe "$GKE_CLUSTER" \
  --region "$REGION" --project "$PROJECT" \
  --format="value(endpoint,masterAuth.clusterCaCertificate)")

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
    user: ${GKE_CLUSTER}
    namespace: ${K8S_NAMESPACE}
  name: ${GKE_CLUSTER}
current-context: ${GKE_CLUSTER}
users:
- name: ${GKE_CLUSTER}
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: gke-gcloud-auth-plugin
      installHint: Install gke-gcloud-auth-plugin for use with kubectl by following https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl#install_plugin
      provideClusterInfo: true"

# ── 4. Derive IAP audience ──────────────────────────────────────────────────
# For Cloud Run's direct IAP integration, the audience is:
#   /projects/PROJECT_NUMBER/locations/REGION/services/SERVICE_NAME
# No backend service needs to exist — the audience is deterministic.

PROJECT_NUMBER=$(gcloud projects describe "$PROJECT" --format="value(projectNumber)")
IAP_AUDIENCE="/projects/${PROJECT_NUMBER}/locations/${REGION}/services/${SERVICE_NAME}"
log "IAP audience: ${IAP_AUDIENCE}"

# ── 5. Generate hub settings ────────────────────────────────────────────────

SESSION_SECRET="${SCION_SESSION_SECRET:-$(openssl rand -hex 32)}"

SETTINGS_CONTENT=$(sed \
  -e "s|__SESSION_SECRET__|${SESSION_SECRET}|" \
  -e "s|__IAP_AUDIENCE__|${IAP_AUDIENCE}|" \
  -e "s|__TRANSPORT_SA_EMAIL__|${TRANSPORT_SA_EMAIL}|" \
  "${SCRIPT_DIR}/hub-settings-template.yaml")

# ── 6. Store secrets ────────────────────────────────────────────────────────

log "Storing secrets in Secret Manager"
ensure_secret "${SERVICE_NAME}-kubeconfig" "$KUBECONFIG_CONTENT"
ensure_secret "${SERVICE_NAME}-settings"   "$SETTINGS_CONTENT"

# ── 7. Ensure K8s namespace ─────────────────────────────────────────────────

log "Ensuring namespace ${K8S_NAMESPACE} exists in ${GKE_CLUSTER}"
LOCAL_KUBECONFIG=$(mktemp)
echo "$KUBECONFIG_CONTENT" > "$LOCAL_KUBECONFIG"
KUBECONFIG="$LOCAL_KUBECONFIG" kubectl create namespace "$K8S_NAMESPACE" --dry-run=client -o yaml | KUBECONFIG="$LOCAL_KUBECONFIG" kubectl apply -f - || true
rm -f "$LOCAL_KUBECONFIG"

# ── 8. Create Artifact Registry repo (if needed) ────────────────────────────

if ! gcloud artifacts repositories describe "$REPO" \
  --location="$REGION" --project="$PROJECT" &>/dev/null; then
  log "Creating Artifact Registry repository ${REPO}"
  gcloud artifacts repositories create "$REPO" \
    --repository-format=docker \
    --location="$REGION" \
    --project="$PROJECT"
fi

# ── 9. Deploy Cloud Run service with IAP enabled ────────────────────────────

log "Deploying Cloud Run service ${SERVICE_NAME} with IAP"
gcloud run deploy "$SERVICE_NAME" \
  --image "$IMAGE" \
  --region "$REGION" \
  --project "$PROJECT" \
  --min-instances 1 \
  --max-instances 1 \
  --no-allow-unauthenticated \
  --iap \
  --no-cpu-throttling \
  --service-account "$SA_EMAIL" \
  --port 8080 \
  --memory 1Gi \
  --cpu 1 \
  --timeout 3600 \
  --set-secrets "/home/scion/.kube/config=${SERVICE_NAME}-kubeconfig:latest,/run/secrets/settings.yaml=${SERVICE_NAME}-settings:latest" \
  --set-env-vars "HOME=/home/scion,KUBECONFIG=/home/scion/.kube/config"

SERVICE_URL=$(gcloud run services describe "$SERVICE_NAME" \
  --region "$REGION" --project "$PROJECT" \
  --format="value(status.url)")

# ── 10. Grant IAP service agent invoker permission ──────────────────────────
# The IAP service agent needs roles/run.invoker to forward authenticated
# requests to the Cloud Run service.

log "Granting IAP service agent invoker permission"
gcloud run services add-iam-policy-binding "$SERVICE_NAME" \
  --region "$REGION" \
  --project "$PROJECT" \
  --member "serviceAccount:service-${PROJECT_NUMBER}@gcp-sa-iap.iam.gserviceaccount.com" \
  --role "roles/run.invoker"

# ── 11. Configure custom OAuth client (if provided) ─────────────────────────
# By default, Cloud Run IAP uses a Google-managed OAuth client. If custom
# credentials are provided (needed for external users), configure them via
# IAP settings.

if [[ -n "$IAP_CLIENT_ID" && -n "$IAP_CLIENT_SECRET" ]]; then
  log "Configuring custom OAuth client for IAP"
  IAP_SETTINGS_FILE=$(mktemp)
  cat > "$IAP_SETTINGS_FILE" <<YAML
accessSettings:
  oauthSettings:
    clientId: "${IAP_CLIENT_ID}"
    clientSecret: "${IAP_CLIENT_SECRET}"
YAML
  gcloud iap settings set "$IAP_SETTINGS_FILE" \
    --project="$PROJECT" \
    --region="$REGION" \
    --resource-type=cloud-run \
    --service="$SERVICE_NAME"
  rm -f "$IAP_SETTINGS_FILE"
else
  log "Using Google-managed OAuth client for IAP (no custom credentials provided)"
fi

# ── 12. Grant transport SA access through IAP ──────────────────────────────
# The transport SA needs roles/iap.httpsResourceAccessor so agents can call
# back through IAP.

log "Granting transport SA IAP access"
gcloud iap web add-iam-policy-binding \
  --project="$PROJECT" \
  --region="$REGION" \
  --resource-type=cloud-run \
  --service="$SERVICE_NAME" \
  --member="serviceAccount:${TRANSPORT_SA_EMAIL}" \
  --role="roles/iap.httpsResourceAccessor"

# ── 13. Print summary ───────────────────────────────────────────────────────

log "Deployment complete"
echo ""
echo "  Cloud Run URL (IAP-protected): ${SERVICE_URL}"
echo "  IAP audience: ${IAP_AUDIENCE}"
echo "  Transport SA: ${TRANSPORT_SA_EMAIL}"
echo ""
echo "  IAP protects all ingress — users will be redirected to Google sign-in."
echo ""
echo "  Direct health check (bypasses IAP):"
echo "    curl -H \"Authorization: Bearer \$(gcloud auth print-identity-token)\" ${SERVICE_URL}/health"
echo ""
echo "  Grant a user IAP access:"
echo "    gcloud iap web add-iam-policy-binding \\"
echo "      --project=${PROJECT} --region=${REGION} \\"
echo "      --resource-type=cloud-run --service=${SERVICE_NAME} \\"
echo "      --member=user:EMAIL --role=roles/iap.httpsResourceAccessor"
echo ""
