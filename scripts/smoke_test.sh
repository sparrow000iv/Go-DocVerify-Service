#!/usr/bin/env bash
# End-to-end smoke test against a running DocVerify instance.
# Usage: ./scripts/smoke_test.sh [base_url]
#   default base_url: http://localhost:8080
set -euo pipefail

BASE="${1:-http://localhost:8080}"
PASS=0
FAIL=0

green() { printf '\033[0;32m%s\033[0m\n' "$1"; }
red()   { printf '\033[0;31m%s\033[0m\n' "$1"; }

# check <name> <expected_code> <curl args...>
check() {
  local name="$1" expected="$2"; shift 2
  local code
  code=$(curl -s -o /tmp/smoke_body -w '%{http_code}' "$@")
  if [[ "$code" == "$expected" ]]; then
    green "  PASS  ${name} (HTTP ${code})"
    PASS=$((PASS + 1))
  else
    red   "  FAIL  ${name} (got HTTP ${code}, want ${expected})"
    red   "        body: $(cat /tmp/smoke_body)"
    FAIL=$((FAIL + 1))
  fi
}

echo "Smoke testing ${BASE}"
echo

echo "Health and observability"
check "healthz responds"        200 "${BASE}/healthz"
check "readyz responds"         200 "${BASE}/readyz"
check "metrics exposed"         200 "${BASE}/metrics"

echo
echo "Happy path"
DOC_ID=$(curl -s -X POST "${BASE}/api/v1/documents" \
  -H 'Content-Type: application/json' \
  -d '{"owner":"smoke","doc_type":"passport","content":"smoke-payload"}' \
  | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')

if [[ -n "$DOC_ID" ]]; then
  green "  PASS  document created (${DOC_ID})"
  PASS=$((PASS + 1))
else
  red   "  FAIL  could not create document"
  FAIL=$((FAIL + 1))
  exit 1
fi

check "fetch document"          200 "${BASE}/api/v1/documents/${DOC_ID}"
check "verify document"         200 -X POST "${BASE}/api/v1/documents/${DOC_ID}/verify"
check "verify is idempotent"    200 -X POST "${BASE}/api/v1/documents/${DOC_ID}/verify"
check "list documents"          200 "${BASE}/api/v1/documents"
check "list with limit"         200 "${BASE}/api/v1/documents?limit=1"

echo
echo "Negative paths"
check "empty owner rejected"    400 -X POST "${BASE}/api/v1/documents" \
      -H 'Content-Type: application/json' -d '{"owner":"","doc_type":"passport","content":"x"}'
check "bad doc_type rejected"   400 -X POST "${BASE}/api/v1/documents" \
      -H 'Content-Type: application/json' -d '{"owner":"t","doc_type":"library_card","content":"x"}'
check "malformed JSON rejected" 400 -X POST "${BASE}/api/v1/documents" \
      -H 'Content-Type: application/json' -d '{"owner":'
check "unknown id is 404"       404 "${BASE}/api/v1/documents/does-not-exist"
check "bad status filter"       400 "${BASE}/api/v1/documents?status=BOGUS"

echo
echo "Cleanup"
check "delete document"         204 -X DELETE "${BASE}/api/v1/documents/${DOC_ID}"
check "deleted doc is gone"     404 "${BASE}/api/v1/documents/${DOC_ID}"

echo
echo "-----------------------------"
echo "Passed: ${PASS}   Failed: ${FAIL}"
if [[ $FAIL -gt 0 ]]; then
  red "SMOKE TEST FAILED"
  exit 1
fi
green "ALL SMOKE TESTS PASSED"
