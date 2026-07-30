#!/usr/bin/env bash
# Cross-compiles core/ for android/arm64 and drops it into app/'s jniLibs so
# Android packages it as an extractable, executable "native lib". The
# lib*.so naming and jniLibs location are load-bearing — see app/README.md
# (or PROJECT.md) for why.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

out_dir="../app/src/main/jniLibs/arm64-v8a"
mkdir -p "$out_dir"

CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -o "$out_dir/libwhatsmeowcore.so" .

echo "built $out_dir/libwhatsmeowcore.so"
