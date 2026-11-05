#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

interop_dir="$(mktemp -d "${TMPDIR:-/tmp}/orc-go-interop.XXXXXX")"
trap 'rm -r "$interop_dir"' EXIT

go run ./interop/write -out "$interop_dir"
python3 ./interop/pyarrow/check.py "$interop_dir"
mvn -f ./interop/java/pom.xml -q compile exec:java "-Dexec.args=\"$interop_dir\""
