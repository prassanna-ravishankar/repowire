#!/usr/bin/env bash
# Manual deploy for repowire.io apex (web/marketing landing).
# Run from repowire repo root.
# Requires: gcloud authed against baldmaninc, docker running, helm installed.
#
# Prereq: charts/repowire-relay/values.yaml must already have `repowire.io`
# REMOVED from hostnames (see #147) AND `helm upgrade repowire-relay` applied.
# Otherwise two HTTPRoutes claim the same host on the same Gateway.

set -euo pipefail

APP_NAME=repowire-web
NAMESPACE=repowire
DOCKERFILE=web-image/Dockerfile
CHART_PATH=charts/repowire-web

PROJECT_ID=baldmaninc
CLUSTER=clusterkit
REGION=us-central1
REGISTRY=us-docker.pkg.dev/${PROJECT_ID}/gcr.io
SHA=$(git rev-parse --short HEAD)

echo "==> SHA=${SHA}  APP=${APP_NAME}  NS=${NAMESPACE}"

echo "==> Configuring Docker for Artifact Registry"
gcloud auth configure-docker us-docker.pkg.dev --quiet

echo "==> Getting GKE credentials"
gcloud container clusters get-credentials "${CLUSTER}" --region "${REGION}" --project "${PROJECT_ID}"

echo "==> Building + pushing ${APP_NAME}"
docker build --platform linux/amd64 \
  -t "${REGISTRY}/${APP_NAME}:${SHA}" \
  -t "${REGISTRY}/${APP_NAME}:latest" \
  -f "${DOCKERFILE}" .
docker push "${REGISTRY}/${APP_NAME}:${SHA}"
docker push "${REGISTRY}/${APP_NAME}:latest"

echo "==> helm upgrade ${APP_NAME}"
helm upgrade --install "${APP_NAME}" "${CHART_PATH}" \
  --namespace "${NAMESPACE}" \
  --set image.tag="${SHA}" \
  --wait --timeout 5m

echo "==> Verifying"
kubectl rollout status "deployment/${APP_NAME}" -n "${NAMESPACE}" --timeout=3m

echo "==> Smoke checks for https://repowire.io"
curl -sS -o /dev/null -w "HTTP %{http_code} in %{time_total}s\n" https://repowire.io || true
echo "--- TLS issuer"
echo | openssl s_client -connect repowire.io:443 -servername repowire.io 2>/dev/null \
  | openssl x509 -noout -issuer -subject 2>/dev/null || true

echo "==> Done."
