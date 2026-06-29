#!/usr/bin/env bash
# Deploy the Scion hub as a production Cloud Run HA service with native IAP.
#
# Architecture:
#   User / agent / CLI -> Cloud Run native IAP -> Hub/Web/co-located Broker
#     -> Cloud SQL Postgres, GCS, Cloud Run Instances, Filestore
#
# This script intentionally does not provision the older SQLite/GKE demo shape.
# Required production inputs are supplied through SCION_* environment variables;
# see scripts/cloudrun/README.md for the full list.

set -euo pipefail

PROJECT="${SCION_PROJECT:-deploy-demo-test}"
REGION="${SCION_REGION:-us-central1}"
SERVICE_NAME="${SCION_SERVICE:-scion-hub}"
SA_NAME="${SCION_SA_NAME:-scion-hub-sa}"
TRANSPORT_SA_NAME="${SCION_TRANSPORT_SA_NAME:-${SA_NAME}-transport}"
RUNTIME_SA_NAME="${SCION_RUNTIME_SA_NAME:-scion-agent-runtime-sa}"
REPO="${SCION_REPO:-scion}"
IMAGE_TAG="${SCION_IMAGE_TAG:-latest}"
IMAGE_REGISTRY="${REGION}-docker.pkg.dev/${PROJECT}/${REPO}"
IMAGE="${IMAGE_REGISTRY}/hub:${IMAGE_TAG}"

CLOUDSQL_INSTANCE="${SCION_CLOUDSQL_INSTANCE:-}"
DATABASE_NAME="${SCION_DATABASE_NAME:-scionhub}"
DATABASE_USER="${SCION_DATABASE_USER:-scion}"
DATABASE_PASSWORD="${SCION_DATABASE_PASSWORD:-}"
DATABASE_PASSWORD_SECRET="${SCION_DATABASE_PASSWORD_SECRET:-}"
DATABASE_URL="${SCION_DATABASE_URL:-}"
DB_MAX_OPEN_CONNS="${SCION_DB_MAX_OPEN_CONNS:-10}"
DB_MAX_IDLE_CONNS="${SCION_DB_MAX_IDLE_CONNS:-5}"

GCS_BUCKET="${SCION_GCS_BUCKET:-}"
RUNTIME_NETWORK="${SCION_RUNTIME_NETWORK:-}"
RUNTIME_SUBNETWORK="${SCION_RUNTIME_SUBNETWORK:-}"
FILESTORE_IP="${SCION_FILESTORE_IP:-}"
FILESTORE_EXPORT="${SCION_FILESTORE_EXPORT:-}"
BROKER_ID="${SCION_BROKER_ID:-cloudrun-instances}"
BROKER_NAME="${SCION_BROKER_NAME:-Cloud Run Instances}"
PUBLIC_URL="${SCION_PUBLIC_URL:-}"

MIN_INSTANCES="${SCION_MIN_INSTANCES:-2}"
MAX_INSTANCES="${SCION_MAX_INSTANCES:-10}"
CPU="${SCION_CPU:-1}"
MEMORY="${SCION_MEMORY:-1Gi}"
TIMEOUT="${SCION_TIMEOUT:-3600}"

IAP_CLIENT_ID="${SCION_IAP_CLIENT_ID:-}"
IAP_CLIENT_SECRET="${SCION_IAP_CLIENT_SECRET:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

SKIP_BUILD=false
for arg in "$@"; do
  case "$arg" in
    --skip-build) SKIP_BUILD=true ;;
    *) echo "ERROR: unknown argument: $arg" >&2; exit 1 ;;
  esac
done

log() { echo "==> $*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

require_var() {
  local name="$1"
  local value="$2"
  [[ -n "$value" ]] || die "${name} is required"
}

ensure_secret() {
  local name="$1"
  local data="$2"
  if gcloud secrets describe "$name" --project="$PROJECT" &>/dev/null; then
    log "Updating secret ${name}"
    printf '%s' "$data" | gcloud secrets versions add "$name" --data-file=- --project="$PROJECT"
  else
    log "Creating secret ${name}"
    printf '%s' "$data" | gcloud secrets create "$name" --data-file=- --project="$PROJECT" \
      --replication-policy=automatic
  fi
}

ensure_service_account() {
  local name="$1"
  local display_name="$2"
  local email="${name}@${PROJECT}.iam.gserviceaccount.com"
  if ! gcloud iam service-accounts describe "$email" --project="$PROJECT" &>/dev/null; then
    log "Creating service account ${name}"
    gcloud iam service-accounts create "$name" \
      --display-name="$display_name" \
      --project="$PROJECT"
  fi
}

add_project_role() {
  local member="$1"
  local role="$2"
  gcloud projects add-iam-policy-binding "$PROJECT" \
    --member="$member" \
    --role="$role" \
    --condition=None \
    --quiet >/dev/null
}

sed_escape() {
  # Escape only \, &, and | (the delimiter used in render_settings).
  # No need to escape / since we use | as the sed delimiter.
  printf '%s' "$1" | sed -e 's/[\\&|]/\\&/g'
}

urlencode() {
  python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1"
}

render_settings() {
  local service_url="$1"
  sed \
    -e "s|__IMAGE_REGISTRY__|$(sed_escape "$IMAGE_REGISTRY")|" \
    -e "s|__SERVICE_URL__|$(sed_escape "$service_url")|" \
    -e "s|__PROJECT_ID__|$(sed_escape "$PROJECT")|" \
    -e "s|__REGION__|$(sed_escape "$REGION")|" \
    -e "s|__POSTGRES_DSN__|$(sed_escape "$DATABASE_URL")|" \
    -e "s|__DB_MAX_OPEN_CONNS__|$(sed_escape "$DB_MAX_OPEN_CONNS")|" \
    -e "s|__DB_MAX_IDLE_CONNS__|$(sed_escape "$DB_MAX_IDLE_CONNS")|" \
    -e "s|__GCS_BUCKET__|$(sed_escape "$GCS_BUCKET")|" \
    -e "s|__IAP_AUDIENCE__|$(sed_escape "$IAP_AUDIENCE")|" \
    -e "s|__TRANSPORT_SA_EMAIL__|$(sed_escape "$TRANSPORT_SA_EMAIL")|" \
    -e "s|__BROKER_ID__|$(sed_escape "$BROKER_ID")|" \
    -e "s|__BROKER_NAME__|$(sed_escape "$BROKER_NAME")|" \
    -e "s|__RUNTIME_SA_EMAIL__|$(sed_escape "$RUNTIME_SA_EMAIL")|" \
    -e "s|__RUNTIME_NETWORK__|$(sed_escape "$RUNTIME_NETWORK")|" \
    -e "s|__RUNTIME_SUBNETWORK__|$(sed_escape "$RUNTIME_SUBNETWORK")|" \
    -e "s|__FILESTORE_IP__|$(sed_escape "$FILESTORE_IP")|" \
    -e "s|__FILESTORE_EXPORT__|$(sed_escape "$FILESTORE_EXPORT")|" \
    "${SCRIPT_DIR}/hub-settings-template.yaml"
}

deploy_service() {
  local settings_secret="$1"
  local session_secret="$2"

  gcloud run deploy "$SERVICE_NAME" \
    --image "$IMAGE" \
    --region "$REGION" \
    --project "$PROJECT" \
    --min-instances "$MIN_INSTANCES" \
    --max-instances "$MAX_INSTANCES" \
    --no-allow-unauthenticated \
    --iap \
    --no-cpu-throttling \
    --add-cloudsql-instances "$CLOUDSQL_CONNECTION_NAME" \
    --service-account "$SA_EMAIL" \
    --port 8080 \
    --memory "$MEMORY" \
    --cpu "$CPU" \
    --timeout "$TIMEOUT" \
    --set-secrets "/run/secrets/settings.yaml=${settings_secret}:latest,SCION_SERVER_SESSION_SECRET=${session_secret}:latest" \
    --set-env-vars "HOME=/home/scion,SCION_REQUIRE_STABLE_SIGNING_KEY=true"
}

command -v gcloud >/dev/null || die "gcloud CLI not found"
command -v docker >/dev/null || die "docker CLI not found"
command -v python3 >/dev/null || die "python3 is required to URL-encode the database password"
command -v openssl >/dev/null || die "openssl is required to generate the session secret"

require_var "SCION_CLOUDSQL_INSTANCE" "$CLOUDSQL_INSTANCE"
require_var "SCION_GCS_BUCKET" "$GCS_BUCKET"
require_var "SCION_RUNTIME_NETWORK" "$RUNTIME_NETWORK"
require_var "SCION_RUNTIME_SUBNETWORK" "$RUNTIME_SUBNETWORK"
require_var "SCION_FILESTORE_IP" "$FILESTORE_IP"
require_var "SCION_FILESTORE_EXPORT" "$FILESTORE_EXPORT"

SA_EMAIL="${SA_NAME}@${PROJECT}.iam.gserviceaccount.com"
TRANSPORT_SA_EMAIL="${TRANSPORT_SA_NAME}@${PROJECT}.iam.gserviceaccount.com"
RUNTIME_SA_EMAIL="${RUNTIME_SA_NAME}@${PROJECT}.iam.gserviceaccount.com"
CLOUDSQL_CONNECTION_NAME="${PROJECT}:${REGION}:${CLOUDSQL_INSTANCE}"

if [[ -z "$DATABASE_URL" ]]; then
  if [[ -z "$DATABASE_PASSWORD" && -n "$DATABASE_PASSWORD_SECRET" ]]; then
    log "Reading database password from Secret Manager secret ${DATABASE_PASSWORD_SECRET}"
    DATABASE_PASSWORD=$(gcloud secrets versions access latest \
      --secret="$DATABASE_PASSWORD_SECRET" \
      --project="$PROJECT")
  fi
  require_var "SCION_DATABASE_PASSWORD or SCION_DATABASE_URL" "$DATABASE_PASSWORD"
  ENCODED_DB_PASSWORD="$(urlencode "$DATABASE_PASSWORD")"
  DATABASE_URL="postgres://${DATABASE_USER}:${ENCODED_DB_PASSWORD}@/${DATABASE_NAME}?host=/cloudsql/${CLOUDSQL_CONNECTION_NAME}"
fi

PROJECT_NUMBER=$(gcloud projects describe "$PROJECT" --format="value(projectNumber)")
IAP_AUDIENCE="/projects/${PROJECT_NUMBER}/locations/${REGION}/services/${SERVICE_NAME}"
log "Cloud Run native IAP audience: ${IAP_AUDIENCE}"

if [[ -z "$PUBLIC_URL" ]]; then
  PUBLIC_URL=$(gcloud run services describe "$SERVICE_NAME" \
    --region "$REGION" \
    --project "$PROJECT" \
    --format="value(status.url)" 2>/dev/null || true)
fi
if [[ -z "$PUBLIC_URL" ]]; then
  PUBLIC_URL="https://pending-first-deploy.invalid"
fi

ensure_service_account "$SA_NAME" "Scion Hub (Cloud Run HA)"
ensure_service_account "$TRANSPORT_SA_NAME" "Scion transport auth (IAP)"
ensure_service_account "$RUNTIME_SA_NAME" "Scion agent runtime (Cloud Run Instances)"

log "Granting project IAM roles"
for role in \
  roles/cloudsql.client \
  roles/secretmanager.secretAccessor \
  roles/storage.objectAdmin \
  roles/run.admin \
  roles/logging.viewer \
  roles/iap.tunnelResourceAccessor; do
  add_project_role "serviceAccount:${SA_EMAIL}" "$role"
done
add_project_role "serviceAccount:${SA_EMAIL}" "roles/iam.serviceAccountUser"

log "Granting hub SA token minting access on ${TRANSPORT_SA_EMAIL}"
gcloud iam service-accounts add-iam-policy-binding "$TRANSPORT_SA_EMAIL" \
  --member="serviceAccount:${SA_EMAIL}" \
  --role="roles/iam.serviceAccountTokenCreator" \
  --project="$PROJECT" \
  --quiet >/dev/null

log "Granting hub SA runtime service account attachment access"
gcloud iam service-accounts add-iam-policy-binding "$RUNTIME_SA_EMAIL" \
  --member="serviceAccount:${SA_EMAIL}" \
  --role="roles/iam.serviceAccountUser" \
  --project="$PROJECT" \
  --quiet >/dev/null

if ! gcloud artifacts repositories describe "$REPO" \
  --location="$REGION" \
  --project="$PROJECT" &>/dev/null; then
  log "Creating Artifact Registry repository ${REPO}"
  gcloud artifacts repositories create "$REPO" \
    --repository-format=docker \
    --location="$REGION" \
    --project="$PROJECT"
fi

if [[ "$SKIP_BUILD" == false ]]; then
  log "Building container image ${IMAGE}"
  docker build -f "${SCRIPT_DIR}/Dockerfile" -t "$IMAGE" "$REPO_ROOT"

  log "Pushing container image"
  docker push "$IMAGE"
else
  log "Skipping build (--skip-build)"
fi

SETTINGS_SECRET="${SERVICE_NAME}-settings"
SESSION_SECRET_NAME="${SERVICE_NAME}-session-secret"
SESSION_SECRET="${SCION_SESSION_SECRET:-$(openssl rand -hex 32)}"

log "Rendering production settings for ${PUBLIC_URL}"
ensure_secret "$SETTINGS_SECRET" "$(render_settings "$PUBLIC_URL")"
ensure_secret "$SESSION_SECRET_NAME" "$SESSION_SECRET"

log "Deploying Cloud Run service ${SERVICE_NAME}"
deploy_service "$SETTINGS_SECRET" "$SESSION_SECRET_NAME"

SERVICE_URL=$(gcloud run services describe "$SERVICE_NAME" \
  --region "$REGION" \
  --project "$PROJECT" \
  --format="value(status.url)")

if [[ "${SCION_PUBLIC_URL:-}" == "" && "$SERVICE_URL" != "$PUBLIC_URL" ]]; then
  log "Updating settings with discovered Cloud Run URL ${SERVICE_URL}"
  ensure_secret "$SETTINGS_SECRET" "$(render_settings "$SERVICE_URL")"
  deploy_service "$SETTINGS_SECRET" "$SESSION_SECRET_NAME"
fi

log "Granting IAP service agent invoker permission"
gcloud run services add-iam-policy-binding "$SERVICE_NAME" \
  --region "$REGION" \
  --project "$PROJECT" \
  --member "serviceAccount:service-${PROJECT_NUMBER}@gcp-sa-iap.iam.gserviceaccount.com" \
  --role "roles/run.invoker" \
  --quiet >/dev/null

if [[ -n "$IAP_CLIENT_ID" && -n "$IAP_CLIENT_SECRET" ]]; then
  log "Configuring custom OAuth client for Cloud Run IAP"
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
  log "Using Google-managed OAuth client for IAP"
fi

log "Granting transport SA access through IAP"
gcloud iap web add-iam-policy-binding \
  --project="$PROJECT" \
  --region="$REGION" \
  --resource-type=cloud-run \
  --service="$SERVICE_NAME" \
  --member="serviceAccount:${TRANSPORT_SA_EMAIL}" \
  --role="roles/iap.httpsResourceAccessor" \
  --quiet >/dev/null

log "Deployment complete"
echo ""
echo "  Cloud Run URL: ${SERVICE_URL}"
echo "  IAP audience: ${IAP_AUDIENCE}"
echo "  Settings secret: ${SETTINGS_SECRET}"
echo "  Session secret: ${SESSION_SECRET_NAME}"
echo "  Hub service account: ${SA_EMAIL}"
echo "  Transport service account: ${TRANSPORT_SA_EMAIL}"
echo "  Runtime service account: ${RUNTIME_SA_EMAIL}"
echo ""
echo "Grant users access with:"
echo "  gcloud iap web add-iam-policy-binding \\"
echo "    --project=${PROJECT} --region=${REGION} --resource-type=cloud-run --service=${SERVICE_NAME} \\"
echo "    --member=user:EMAIL --role=roles/iap.httpsResourceAccessor"
