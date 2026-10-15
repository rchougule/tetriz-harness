#!/usr/bin/env bash
# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT
# Compare fresh output to the checkout without replacing edited or stale generated files.
set -euo pipefail
cd "$(dirname "$0")/.."
repo="$PWD"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cp buf.yaml buf.gen.yaml "$scratch/"
cp -R proto "$scratch/proto"
if [ -f buf.lock ]; then cp buf.lock "$scratch/"; fi
(cd "$scratch" && "${BUF:-buf}" generate)
# These directories are entirely generated; gen/package.json and tsconfig.json are handwritten.
for language in go ts; do
    if ! diff -ru "$repo/gen/$language" "$scratch/gen/$language"; then
        echo "Generated protobuf $language files differ; run make proto and commit the output." >&2
        exit 1
    fi
done
