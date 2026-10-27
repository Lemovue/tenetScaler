#!/usr/bin/env bash
# Lint: gofmt (must list nothing), go vet, JS syntax check, Python compile check.
set -uo pipefail
cd "$(dirname "$0")/.."
failed=0

dirs=(); for d in cmd internal test; do [ -d "$d" ] && dirs+=("$d"); done
unformatted=$(gofmt -l "${dirs[@]}")
if [ -n "$unformatted" ]; then echo "gofmt: these files need formatting:"; echo "$unformatted"; failed=1; else echo "gofmt: OK"; fi

if go vet -p 4 ./...; then echo "go vet: OK"; else failed=1; fi

js_ok=1
while IFS= read -r f; do node --check "$f" || { js_ok=0; failed=1; }; done < <(find internal/web/static -name '*.js')
[ "$js_ok" = 1 ] && echo "node --check: OK"

if python3 -m py_compile scripts/dev/resource_guard.py scripts/dev/heavy.py; then echo "py_compile: OK"; else failed=1; fi

if [ "$failed" = 1 ]; then echo "LINT FAILED"; exit 1; fi
echo "LINT OK"
