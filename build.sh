#!/bin/bash

# build.sh — build llama-swappy (see README.md).
# Default output: llama-swappy in the repository root.
# Optional output path: ./build.sh /some/dir/llama-swappy

set -euo pipefail
cd "$(dirname "$0")"

out="${1:-llama-swappy}"
go build -o "$out" .
echo "Built $out"
