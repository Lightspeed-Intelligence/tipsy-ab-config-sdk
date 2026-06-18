#!/usr/bin/env bash
set -euo pipefail

# Regenerate Go protobuf + gRPC stubs for the 2 SDK-facing protos in this repo:
#   api/proto/tipsy/config/v1/config.proto
#   api/proto/tipsy/abtest/v1/abtest.proto
#
# The 3 backend-private protos (config/v1/internal.proto, abtest/v1/internal.proto,
# audit/v1/audit.proto) live in the private tipsy-ab-config repo and are NOT
# regenerated here. Each repo owns its own gen-proto.sh.
#
# Canonical tool versions (must match what produced the checked-in *.pb.go;
# CI's proto drift check will fail if a newer version of any tool reformats
# the output):
#   protoc             v7.34.1
#   protoc-gen-go      v1.36.11
#   protoc-gen-go-grpc v1.6.2
#
# Lockstep bump: when changing any of these here, the same bump must land in
# the private tipsy-ab-config repo's scripts/gen-proto.sh in the same window.

# Front-check required tools. If any are missing, print install hints and exit.
missing=()
for tool in protoc protoc-gen-go protoc-gen-go-grpc; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    missing+=("$tool")
  fi
done

if [ ${#missing[@]} -gt 0 ]; then
  echo "Missing required tools: ${missing[*]}" >&2
  echo "" >&2
  echo "Install hints:" >&2
  for tool in "${missing[@]}"; do
    case "$tool" in
      protoc)
        echo "  protoc: install from https://github.com/protocolbuffers/protobuf/releases" >&2
        ;;
      protoc-gen-go)
        echo "  go install google.golang.org/protobuf/cmd/protoc-gen-go@latest" >&2
        ;;
      protoc-gen-go-grpc)
        echo "  go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest" >&2
        ;;
    esac
  done
  echo "" >&2
  echo "After 'go install', ensure \$(go env GOPATH)/bin is on PATH." >&2
  exit 1
fi

cd "$(dirname "$0")/.."

# Proto sources live in api/proto/tipsy/<module>/<version>/. Each .proto
# declares an explicit go_package option pointing into api/gen/go/tipsy/<module>/v1/.
# We invoke protoc with paths=import so the generated files land at the path
# expressed by go_package, then we relocate from the module-prefixed scratch
# directory into the repo tree.

PROTO_FILES=(
  api/proto/tipsy/config/v1/config.proto
  api/proto/tipsy/abtest/v1/abtest.proto
)

for f in "${PROTO_FILES[@]}"; do
  if [ ! -f "$f" ]; then
    echo "Proto source not found: $f" >&2
    exit 1
  fi
done

protoc \
  -I api/proto \
  --go_out=. --go_opt=paths=import \
  --go-grpc_out=. --go-grpc_opt=paths=import \
  "${PROTO_FILES[@]}"

# protoc with paths=import drops generated files under
# ./github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/... — relocate into
# the real repo tree (api/gen/go/...) and clean up the scratch prefix.
if [ -d github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk ]; then
  cp -r github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/* .
  rm -rf github.com
fi

echo "Generated proto bindings:"
echo "  config/abtest stubs -> api/gen/go/tipsy/{config,abtest}/v1/"
for f in "${PROTO_FILES[@]}"; do
  echo "  source: $f"
done
