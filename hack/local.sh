#!/usr/bin/env bash
# Operates only on the dedicated sandbox-cloud-dev cluster and isolated config.
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$PWD/.local/bin:$PATH"
cluster=sandbox-cloud-dev
config="$PWD/.local/kubeconfig"
context="kind-$cluster"
mkdir -p .local
k() { kubectl --kubeconfig "$config" --context "$context" "$@"; }
case "${1:-}" in
  up)
    command -v kind >/dev/null
    docker info >/dev/null
    if ! kind get clusters | grep -qx "$cluster"; then
      kind create cluster --name "$cluster" --image kindest/node:v1.35.0@sha256:452d707d4862f52530247495d180205e029056831160e22870e37e3f6c1ac31f --config hack/kind.yaml --kubeconfig "$config" --wait 120s
    else
      kind export kubeconfig --name "$cluster" --kubeconfig "$config"
    fi
    docker build --target control -t sandbox-cloud-control:dev .
    docker build --target workspace -t sandbox-cloud-workspace:dev .
    kind load docker-image --name "$cluster" sandbox-cloud-control:dev sandbox-cloud-workspace:dev
    k apply -f deploy/foundation.yaml
    # Tag-based images are confined to this local script. Cloud rendering still requires digests.
    sed -e 's|CONTROL_IMAGE|sandbox-cloud-control:dev|' -e 's|WORKSPACE_IMAGE|sandbox-cloud-workspace:dev|' deploy/api.yaml.template > .local/api.yaml
    k apply -f .local/api.yaml
    k rollout restart deployment/sandbox-api -n sandbox-control
    k rollout status deployment/sandbox-api -n sandbox-control --timeout=180s
    echo 'Ready. Run make local-forward, then use ./bin/sandbox --api http://127.0.0.1:18080'
    ;;
  forward)
    exec kubectl --kubeconfig "$config" --context "$context" -n sandbox-control port-forward --address=127.0.0.1 service/sandbox-api 18080:8080
    ;;
  test)
    k -n sandbox-control port-forward --address=127.0.0.1 service/sandbox-api 18080:8080 > .local/port-forward.log 2>&1 &
    forward_pid=$!
    trap 'kill "$forward_pid" 2>/dev/null || true' EXIT
    ready=false
    for ((i=0; i<60; i++)); do
      kill -0 "$forward_pid" 2>/dev/null || { cat .local/port-forward.log; exit 1; }
      if curl --connect-timeout 1 --max-time 2 --fail --silent http://127.0.0.1:18080/healthz >/dev/null; then ready=true; break; fi
      sleep 1
    done
    if [ "$ready" != true ]; then
      echo 'Timed out waiting for the local API' >&2
      cat .local/port-forward.log >&2
      exit 1
    fi
    SANDBOX_E2E_API=http://127.0.0.1:18080 SANDBOX_E2E_KUBECONFIG="$config" SANDBOX_E2E_CONTEXT="$context" go test -tags=e2e -count=1 -v -timeout=8m ./tests/e2e
    ;;
  down)
    kind delete cluster --name "$cluster" --kubeconfig "$config"
    ;;
  *) echo 'Usage: bash hack/local.sh {up|forward|test|down}' >&2; exit 2 ;;
esac
