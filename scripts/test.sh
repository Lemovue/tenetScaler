#!/usr/bin/env bash
# Run all tests: unit tests (parallel packages capped at 4), then the
# end-to-end smoke tests against the running app (single worker).
set -euo pipefail
cd "$(dirname "$0")/.."
export GOMAXPROCS=8

go test -p 4 -count=1 -timeout 600s $(go list ./... | grep -v /test/e2e)
if [ -d test/e2e ]; then
  go test -p 1 -count=1 -timeout 900s -v ./test/e2e/...
fi
echo "ALL TESTS PASSED"
