#!/usr/bin/env bash
# F29: regenerate dist/chart/templates/crd/*.yaml from config/crd/bases (the
# controller-gen output). The chart wraps each CRD in a `crd.enabled` guard
# plus a keep-annotation block; this keeps that header and replaces everything
# from `spec:` on with the generated spec, so the chart CRD cannot drift from
# the Go types (it had: status.progress.lessonsAvailable, spec.agent.tmpSizeLimit
# and status.agent were missing, so the API server pruned them on a Helm install).
#
# Usage: hack/chart-crd-sync.sh   (run after `make manifests`)
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
for base in "$ROOT"/config/crd/bases/coxswain.wattu.com_*.yaml; do
	plural="$(basename "$base" .yaml)"
	plural="${plural#coxswain.wattu.com_}"
	chart="$ROOT/dist/chart/templates/crd/${plural}.coxswain.wattu.com.yaml"
	[ -f "$chart" ] || { echo "Error: no chart template for $plural ($chart)"; exit 1; }
	tmp="$(mktemp)"
	# header: chart file up to (not including) its `spec:` line
	awk '/^spec:/{exit} {print}' "$chart" > "$tmp"
	# body: generated spec onward
	awk '/^spec:/{p=1} p{print}' "$base" >> "$tmp"
	printf '{{- end }}\n' >> "$tmp"
	mv "$tmp" "$chart"
	echo "synced $chart"
done
