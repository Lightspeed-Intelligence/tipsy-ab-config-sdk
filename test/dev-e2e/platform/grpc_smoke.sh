#!/usr/bin/env bash
# ST3 raw-gRPC smoke test via grpcurl. Reproduces the dev-http-token.md commands
# against the seeded dev topology AND asserts a couple of KNOWN results.
#
# Dev gRPC access (docs/dev-http-token.md §gRPC 接入):
#   - dedicated Cloudflare-proxied gRPC domain with standard TLS
#   - auth metadata: "authorization: Bearer <token>"
#   - the deprecated direct-IP form needs -authority + -insecure; if env vars
#     opt back into IP-direct (AB_CONFIG_GRPC_AUTHORITY set), both flags are
#     added automatically.
#
# Env vars (never hard-code secrets):
#   AB_CONFIG_GRPC_ADDR       (default dev-ab-config-grpc.infra.fantacy.live:443)
#   AB_CONFIG_GRPC_AUTHORITY  (legacy override; if set, switches to IP-direct flags)
#   AB_CONFIG_TOKEN           (REQUIRED)
#
# Run:  AB_CONFIG_TOKEN=... bash test/dev-e2e/platform/grpc_smoke.sh
#
# Exits non-zero on any failed assertion. Requires grpcurl on PATH
# (go install github.com/fullstorydev/grpcurl/cmd/grpcurl@latest).

set -u

GRPC_ADDR="${AB_CONFIG_GRPC_ADDR:-dev-ab-config-grpc.infra.fantacy.live:443}"
GRPC_AUTHORITY="${AB_CONFIG_GRPC_AUTHORITY:-}"
TOKEN="${AB_CONFIG_TOKEN:-}"

CONFIG_SVC="tipsy.config.v1.ConfigService"
ABTEST_SVC="tipsy.abtest.v1.AbtestService"

PASS=0
FAIL=0

pass() { PASS=$((PASS + 1)); printf 'PASS  %s\n' "$1"; }
fail() {
	FAIL=$((FAIL + 1))
	printf 'FAIL  %s\n' "$1"
}

if ! command -v grpcurl >/dev/null 2>&1; then
	echo "SKIP: grpcurl not found on PATH."
	echo "      install: go install github.com/fullstorydev/grpcurl/cmd/grpcurl@latest"
	echo "      (国内可加 GOPROXY=https://goproxy.cn,direct)"
	exit 0
fi

if [ -z "$TOKEN" ]; then
	echo "FATAL: AB_CONFIG_TOKEN env var is required (see docs/dev-http-token.md)"
	exit 2
fi

# jq is preferred for robust JSON assertions; fall back to grep if missing.
HAVE_JQ=0
if command -v jq >/dev/null 2>&1; then
	HAVE_JQ=1
fi

echo "================================================================"
echo "ST3 raw-gRPC smoke (grpcurl)"
echo "  addr      : $GRPC_ADDR"
if [ -n "$GRPC_AUTHORITY" ]; then
	echo "  authority : $GRPC_AUTHORITY (legacy IP-direct fallback)"
fi
echo "  jq        : $([ "$HAVE_JQ" = 1 ] && echo yes || echo 'no (grep fallback)')"
echo "  WARNING   : hitting the SHARED dev environment"
echo "================================================================"

# Default: standard TLS to the gRPC domain — no flags beyond Bearer auth.
# Legacy: when AB_CONFIG_GRPC_AUTHORITY is set, restore -insecure + -authority
# for the deprecated IP-direct path.
if [ -n "$GRPC_AUTHORITY" ]; then
	GRPC_FLAGS=(-insecure -authority "$GRPC_AUTHORITY" -H "authorization: Bearer $TOKEN")
else
	GRPC_FLAGS=(-H "authorization: Bearer $TOKEN")
fi

call() {
	# call <data-json> <fully-qualified-method>
	grpcurl "${GRPC_FLAGS[@]}" -d "$1" "$GRPC_ADDR" "$2" 2>&1
}

# ---- 1. reflection list (must include ConfigService + AbtestService) --------
LIST_OUT="$(grpcurl "${GRPC_FLAGS[@]}" "$GRPC_ADDR" list 2>&1)"
if echo "$LIST_OUT" | grep -q "$CONFIG_SVC" && echo "$LIST_OUT" | grep -q "$ABTEST_SVC"; then
	pass "reflection list contains ConfigService and AbtestService"
else
	fail "reflection list missing services; output:
$LIST_OUT"
fi

# ---- 2. ListNamespacesByKind returns the two seeded namespaces -------------
NS_OUT="$(call '{"kind":"NAMESPACE_KIND_BUSINESS"}' "$CONFIG_SVC/ListNamespacesByKind")"
if echo "$NS_OUT" | grep -q '"demo-test"' && echo "$NS_OUT" | grep -q '"for_dev_agent_test"'; then
	pass "ListNamespacesByKind includes demo-test + for_dev_agent_test"
else
	fail "ListNamespacesByKind missing seeded namespaces; output:
$NS_OUT"
fi

# ---- 3. GetStaticConfig demo-test welcome_text == welcome-FULL -------------
STATIC_OUT="$(call '{"namespace":"demo-test","keys":["welcome_text"]}' "$CONFIG_SVC/GetStaticConfig")"
if [ "$HAVE_JQ" = 1 ]; then
	V="$(echo "$STATIC_OUT" | jq -r '.values.welcome_text // empty' 2>/dev/null)"
	if [ "$V" = "welcome-FULL" ]; then
		pass "GetStaticConfig demo-test welcome_text = welcome-FULL"
	else
		fail "GetStaticConfig demo-test welcome_text = '$V' (want welcome-FULL); output:
$STATIC_OUT"
	fi
else
	if echo "$STATIC_OUT" | grep -q 'welcome-FULL'; then
		pass "GetStaticConfig demo-test welcome_text contains welcome-FULL"
	else
		fail "GetStaticConfig demo-test welcome_text missing welcome-FULL; output:
$STATIC_OUT"
	fi
fi

# ---- 4. GetExperimentResult demo-test u0 → welcome_text version 900200011 --
# u0 is a bucketfind-derived UID for E_cfg group A (welcome-A = version
# 900200011); config_flat_kv carries the experiment version id.
ER_OUT="$(call '{"namespace":"demo-test","userId":"u0","experimentType":"EXPERIMENT_TYPE_CONFIG_VERSION","displayType":"RESULT_DISPLAY_TYPE_FLAT_KV"}' "$ABTEST_SVC/GetExperimentResult")"
if [ "$HAVE_JQ" = 1 ]; then
	# config_flat_kv values are int64 → JSON strings in protojson.
	WT="$(echo "$ER_OUT" | jq -r '.configFlatKv.welcome_text // .config_flat_kv.welcome_text // empty' 2>/dev/null)"
	if [ "$WT" = "900200011" ]; then
		pass "GetExperimentResult demo-test u0 config_flat_kv welcome_text = 900200011 (E_cfg group A)"
	else
		fail "GetExperimentResult demo-test u0 welcome_text version = '$WT' (want 900200011); output:
$ER_OUT"
	fi
else
	if echo "$ER_OUT" | grep -q '900200011'; then
		pass "GetExperimentResult demo-test u0 contains version 900200011 (E_cfg group A)"
	else
		fail "GetExperimentResult demo-test u0 missing version 900200011; output:
$ER_OUT"
	fi
fi

# ---- 5. GetDynamicConfig demo-test u0 welcome_text == welcome-A ------------
DYN_OUT="$(call '{"namespace":"demo-test","userId":"u0","keys":["welcome_text"]}' "$CONFIG_SVC/GetDynamicConfig")"
if [ "$HAVE_JQ" = 1 ]; then
	DV="$(echo "$DYN_OUT" | jq -r '.values.welcome_text // empty' 2>/dev/null)"
	if [ "$DV" = "welcome-A" ]; then
		pass "GetDynamicConfig demo-test u0 welcome_text = welcome-A (resolved value)"
	else
		fail "GetDynamicConfig demo-test u0 welcome_text = '$DV' (want welcome-A); output:
$DYN_OUT"
	fi
else
	if echo "$DYN_OUT" | grep -q 'welcome-A'; then
		pass "GetDynamicConfig demo-test u0 welcome_text contains welcome-A"
	else
		fail "GetDynamicConfig demo-test u0 missing welcome-A; output:
$DYN_OUT"
	fi
fi

echo "----------------------------------------------------------------"
echo "SUMMARY: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
	exit 1
fi
