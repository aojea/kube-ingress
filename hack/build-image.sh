#!/bin/bash

set -o errexit -o nounset -o pipefail

# cd to the repo root
REPO_ROOT=$(git rev-parse --show-toplevel)
cd "${REPO_ROOT}"

docker build . -t kubernetes-sigs/networking/kube-ingress:"${1:-test}"
