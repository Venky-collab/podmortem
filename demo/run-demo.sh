#!/usr/bin/env bash
# Spins up a local kind cluster, deploys four broken apps, and runs podmortem
# against them with a local Ollama model. Everything runs on your machine.
#
#   ./demo/run-demo.sh          # start
#   ./demo/run-demo.sh down     # delete the cluster
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=podmortem-demo
MODEL=${PODMORTEM_MODEL:-qwen2.5:1.5b}

if [[ "${1:-}" == "down" ]]; then
  kind delete cluster --name "$CLUSTER"
  exit 0
fi

need() { command -v "$1" >/dev/null || { echo "missing: $1 (see README > Prerequisites)"; exit 1; }; }
need docker; need kind; need kubectl; need ollama; need go

docker info >/dev/null 2>&1 || { echo "Docker isn't running. With Colima: colima start --memory 3"; exit 1; }
curl -sf localhost:11434/api/version >/dev/null || { echo "Ollama isn't running: ollama serve"; exit 1; }
ollama list | grep -q "^${MODEL}" || ollama pull "$MODEL"

kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --wait 120s
kubectl config use-context "kind-$CLUSTER" >/dev/null

kubectl apply -f demo/broken-apps.yaml
echo "Waiting for the apps to fail (about a minute)..."
sleep 45

go build -o bin/podmortem ./cmd/podmortem
mkdir -p reports
echo
echo "podmortem is watching namespace demo-shop."
echo "Open http://localhost:8080 for reports (also written to ./reports). Ctrl-C to stop."
exec bin/podmortem serve --watch --namespace demo-shop --model "$MODEL" --reports-dir reports
