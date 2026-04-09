#!/usr/bin/env bash

set -euo pipefail
set +H

BLUE='\033[0;34m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

info() { echo -e "${BLUE}[INFO]${NC} $1"; }
success() { echo -e "${GREEN}[SUCCESS]${NC} $1"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
error() { echo -e "${RED}[ERROR]${NC} $1"; }

APP_NAME=${APP_NAME:-"marketflow-ai"}
NAMESPACE=${NAMESPACE:-"marketflow"}
ENV_SECRET_NAME=${ENV_SECRET_NAME:-"marketflow-ai-secrets"}
DEPLOY=${DEPLOY:-true}
SETUP_DATABASES=${SETUP_DATABASES:-true}
DB_TYPES=${DB_TYPES:-postgres,redis}
# Per-service database configuration
SERVICE_DB_NAME=${SERVICE_DB_NAME:-marketflow}
SERVICE_DB_USER=${SERVICE_DB_USER:-marketflow_user}

REGISTRY_SERVER=${REGISTRY_SERVER:-docker.io}
REGISTRY_NAMESPACE=${REGISTRY_NAMESPACE:-codevertex}
IMAGE_REPO="${REGISTRY_SERVER}/${REGISTRY_NAMESPACE}/${APP_NAME}"

DEVOPS_REPO=${DEVOPS_REPO:-"Bengo-Hub/devops-k8s"}
DEVOPS_DIR=${DEVOPS_DIR:-"$HOME/devops-k8s"}
VALUES_FILE_PATH=${VALUES_FILE_PATH:-"apps/${APP_NAME}/values.yaml"}

GIT_EMAIL=${GIT_EMAIL:-"dev@bengobox.com"}
GIT_USER=${GIT_USER:-"MarketFlow AI Bot"}
TRIVY_ECODE=${TRIVY_ECODE:-0}

if [[ -z ${GITHUB_SHA:-} ]]; then
  GIT_COMMIT_ID=$(git rev-parse --short=8 HEAD || echo "localbuild")
else
  GIT_COMMIT_ID=${GITHUB_SHA::8}
fi

info "Service : ${APP_NAME}"
info "Namespace: ${NAMESPACE}"
info "Image   : ${IMAGE_REPO}:${GIT_COMMIT_ID}"

for tool in git docker trivy; do
  command -v "$tool" >/dev/null || { error "$tool is required"; exit 1; }
done
if [[ ${DEPLOY} == "true" ]]; then
  for tool in kubectl helm yq jq; do
    command -v "$tool" >/dev/null || { error "$tool is required"; exit 1; }
  done
fi
success "Prerequisite checks passed"

# =============================================================================
# Auto-sync secrets from devops-k8s
# =============================================================================
if [[ ${DEPLOY} == "true" ]]; then
  info "Checking and syncing required secrets from devops-k8s..."
  SYNC_SCRIPT=$(mktemp)
  if curl -fsSL https://raw.githubusercontent.com/Bengo-Hub/devops-k8s/main/scripts/tools/check-and-sync-secrets.sh -o "$SYNC_SCRIPT" 2>/dev/null; then
    source "$SYNC_SCRIPT"
    check_and_sync_secrets "REGISTRY_USERNAME" "REGISTRY_PASSWORD" "POSTGRES_PASSWORD" "KUBE_CONFIG" || warn "Secret sync failed - continuing with existing secrets"
    rm -f "$SYNC_SCRIPT"
  else
    warn "Unable to download secret sync script - continuing with existing secrets"
  fi
fi

info "Running Trivy filesystem scan"
trivy fs . --exit-code "$TRIVY_ECODE" --format table || true

info "Building Docker image"
DOCKER_BUILDKIT=1 docker build . -t "${IMAGE_REPO}:${GIT_COMMIT_ID}"
success "Docker build complete"

if [[ ${DEPLOY} != "true" ]]; then
  warn "DEPLOY=false -> skipping push/deploy"
  exit 0
fi

if [[ -n ${REGISTRY_USERNAME:-} && -n ${REGISTRY_PASSWORD:-} ]]; then
  echo "$REGISTRY_PASSWORD" | docker login "$REGISTRY_SERVER" -u "$REGISTRY_USERNAME" --password-stdin
fi

docker push "${IMAGE_REPO}:${GIT_COMMIT_ID}"
success "Image pushed"

if [[ -n ${KUBE_CONFIG:-} ]]; then
  mkdir -p ~/.kube
  if echo "$KUBE_CONFIG" | base64 -d > ~/.kube/config 2>/dev/null; then
    info "KUBE_CONFIG decoded from base64"
  else
    echo "$KUBE_CONFIG" > ~/.kube/config
    info "KUBE_CONFIG used as raw content (not base64)"
  fi
  chmod 600 ~/.kube/config
  export KUBECONFIG=~/.kube/config
fi

kubectl get ns "$NAMESPACE" >/dev/null 2>&1 || kubectl create ns "$NAMESPACE"

if [[ -z ${CI:-}${GITHUB_ACTIONS:-} && -f KubeSecrets/devENV.yml ]]; then
  info "Applying local dev secrets"
  kubectl apply -n "$NAMESPACE" -f KubeSecrets/devENV.yml || warn "Failed to apply devENV.yml"
fi

if [[ -n ${REGISTRY_USERNAME:-} && -n ${REGISTRY_PASSWORD:-} ]]; then
  kubectl -n "$NAMESPACE" create secret docker-registry registry-credentials \
    --docker-server="$REGISTRY_SERVER" \
    --docker-username="$REGISTRY_USERNAME" \
    --docker-password="$REGISTRY_PASSWORD" \
    --dry-run=client -o yaml | kubectl apply -f - || warn "registry secret creation failed"
fi

# Create per-service database if SETUP_DATABASES is enabled
if [[ "$SETUP_DATABASES" == "true" && -n "${KUBE_CONFIG:-}" ]]; then
  if kubectl -n infra get statefulset postgresql >/dev/null 2>&1; then
    info "Waiting for PostgreSQL to be ready..."
    kubectl -n infra rollout status statefulset/postgresql --timeout=180s || warn "PostgreSQL not fully ready"

    if [[ -d "$DEVOPS_DIR" ]] || [[ -n "${DEVOPS_REPO:-}" ]]; then
      if [[ ! -d "$DEVOPS_DIR" ]]; then
        TOKEN="${GH_PAT:-${GIT_SECRET:-${GITHUB_TOKEN:-}}}"
        CLONE_URL="https://github.com/${DEVOPS_REPO}.git"
        [[ -n $TOKEN ]] && CLONE_URL="https://x-access-token:${TOKEN}@github.com/${DEVOPS_REPO}.git"
        git clone "$CLONE_URL" "$DEVOPS_DIR" || { warn "Unable to clone devops repo for database setup"; }
      fi

      if [[ -d "$DEVOPS_DIR" && -f "$DEVOPS_DIR/scripts/infrastructure/create-service-database.sh" ]]; then
        info "Creating database '${SERVICE_DB_NAME}' for service ${APP_NAME}..."
        SERVICE_DB_NAME="$SERVICE_DB_NAME" \
        APP_NAME="$APP_NAME" \
        NAMESPACE="$NAMESPACE" \
        bash "$DEVOPS_DIR/scripts/infrastructure/create-service-database.sh" || warn "Database creation failed or already exists"

        # Enable pgvector extension for RAG knowledge base
        info "Enabling pgvector extension on ${SERVICE_DB_NAME}..."
        kubectl exec -n infra postgresql-0 -- psql -U admin_user -d "$SERVICE_DB_NAME" \
          -c "CREATE EXTENSION IF NOT EXISTS vector;" 2>/dev/null || warn "pgvector extension may already exist"
      else
        warn "create-service-database.sh not found - database should be created via devops-k8s infrastructure"
      fi
    fi
  else
    warn "PostgreSQL not found in infra namespace - skipping database creation"
  fi
fi

# Ensure devops-k8s is available for secret creation
if [[ "${DEPLOY}" == "true" && -n "${KUBE_CONFIG:-}" ]] && [[ ! -d "$DEVOPS_DIR" || ! -f "$DEVOPS_DIR/scripts/infrastructure/create-service-secrets.sh" ]]; then
  if [[ ! -d "$DEVOPS_DIR" ]]; then
    info "Cloning devops-k8s for secret creation..."
    TOKEN="${GH_PAT:-${GIT_SECRET:-${GITHUB_TOKEN:-}}}"
    CLONE_URL="https://github.com/${DEVOPS_REPO}.git"
    [[ -n "$TOKEN" ]] && CLONE_URL="https://x-access-token:${TOKEN}@github.com/${DEVOPS_REPO}.git"
    git clone "$CLONE_URL" "$DEVOPS_DIR" || warn "Unable to clone devops repo for secret creation"
  fi
fi

# Ensure service secrets are up-to-date
if [[ -d "$DEVOPS_DIR" && -f "$DEVOPS_DIR/scripts/infrastructure/create-service-secrets.sh" ]]; then
  info "Updating secrets for ${APP_NAME} using devops-k8s script..."
  export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"
  SERVICE_NAME="$APP_NAME" \
  NAMESPACE="$NAMESPACE" \
  DB_NAME="$SERVICE_DB_NAME" \
  DB_USER="$SERVICE_DB_USER" \
  SECRET_NAME="$ENV_SECRET_NAME" \
  POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-}" \
  REDIS_PASSWORD="${REDIS_PASSWORD:-}" \
  bash "$DEVOPS_DIR/scripts/infrastructure/create-service-secrets.sh" || warn "Secret sync failed"
else
  warn "create-service-secrets.sh not available - using existing cluster secrets"
fi

# Update Helm values using centralized script
source "${HOME}/devops-k8s/scripts/helm/update-values.sh" 2>/dev/null || {
  warn "Centralized helm update script not available"
}
if declare -f update_helm_values >/dev/null 2>&1; then
  update_helm_values "$APP_NAME" "$GIT_COMMIT_ID" "$IMAGE_REPO"
else
  warn "update_helm_values function not available - helm values not updated"
fi

info "Deployment summary"
echo "  Image      : ${IMAGE_REPO}:${GIT_COMMIT_ID}"
echo "  Namespace  : ${NAMESPACE}"
echo "  Databases  : ${SETUP_DATABASES} (${DB_TYPES})"
