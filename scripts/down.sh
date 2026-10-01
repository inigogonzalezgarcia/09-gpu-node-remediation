#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=scripts/versions.env
source "$(dirname "$0")/versions.env"
kind delete cluster --name "${CLUSTER_NAME}"
