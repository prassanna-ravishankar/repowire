#!/usr/bin/env bash
# Manual deploy for docs.repowire.io.
# Adapt nothing — run from repowire repo root.
# Requires: gcloud authed against baldmaninc, docker running, helm installed.

set -euo pipefail

APP_NAME=repowire-docs
NAMESPACE=repowire
DOCKERFILE=docs-image/Dockerfile
CHART_PATH=charts/repowire-docs

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

echo "==> Smoke checks for https://docs.repowire.io"
curl -sS -o /dev/null -w "HTTP %{http_code} in %{time_total}s\n" https://docs.repowire.io || true
echo "--- TLS issuer (Cloudflare edge cert; Origin CA validates server-side under Full Strict)"
echo | openssl s_client -connect docs.repowire.io:443 -servername docs.repowire.io 2>/dev/null \
  | openssl x509 -noout -issuer -subject 2>/dev/null || true

echo "==> Done."
