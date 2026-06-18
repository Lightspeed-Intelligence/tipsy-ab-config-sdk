#!/usr/bin/env bash
set -euo pipefail

# Generate Python proto bindings for the Tipsy AB-config SDK.
#
# Outputs land under sdk/python/tipsy_ab_config/_proto/ as a self-contained
# proto package — we deliberately do NOT publish to a system-wide path. The
# SDK ships these generated files with the package; downstream callers only
# need grpcio + protobuf at runtime.
#
# The script reuses the same .proto tree as scripts/gen-proto.sh; only the
# Python output target changes. With only 2 SDK-facing protos in this repo
# (config.proto + abtest.proto), the find-based discovery below picks up
# exactly those two — backend-private internal.proto + audit.proto stayed
# in the private tipsy-ab-config repo and are NOT processed here.
#
# Canonical tool versions (must match what produced the checked-in *_pb2.py
# files; CI's proto-drift check fails if a newer version of any tool
# reformats the output):
#   grpcio-tools         1.66.2   (pinned in pyproject.toml [project.optional-dependencies].dev)
#   protoc (bundled)     same major as the Go-side protoc v7.34.1 family
#                         — grpc_tools.protoc bundles its own protoc binary,
#                         so the host protoc is not consulted here.
#
# Lockstep bump: when changing grpcio-tools here, the same bump must land in
# pyproject.toml's dev extras in the same window. The Go-side gen-proto.sh
# pins protoc separately; keep both in sync when bumping protobuf wire
# behaviour.

cd "$(dirname "$0")/.."

OUT_DIR="sdk/python/tipsy_ab_config/_proto"

# Choose a python with grpcio-tools installed. Default to PYTHON env override,
# fall back to /tmp/sdk-venv (the dev venv created during SDK bootstrap), then
# the system python3.
PYTHON="${PYTHON:-}"
if [ -z "$PYTHON" ]; then
  if [ -x "/tmp/sdk-venv/bin/python" ]; then
    PYTHON="/tmp/sdk-venv/bin/python"
  else
    PYTHON="python3"
  fi
fi

if ! "$PYTHON" -c "import grpc_tools.protoc" 2>/dev/null; then
  echo "grpcio-tools not installed for $PYTHON." >&2
  echo "" >&2
  echo "Install with:" >&2
  echo "  $PYTHON -m pip install grpcio grpcio-tools protobuf" >&2
  echo "" >&2
  echo "Or set PYTHON env var to a venv path that already has grpcio-tools." >&2
  exit 1
fi

mkdir -p "$OUT_DIR"

PROTO_FILES=()
while IFS= read -r f; do
  PROTO_FILES+=("$f")
done < <(find api/proto/tipsy -name '*.proto' -type f | sort)

if [ ${#PROTO_FILES[@]} -eq 0 ]; then
  echo "No proto files found under api/proto/tipsy/." >&2
  exit 1
fi

"$PYTHON" -m grpc_tools.protoc \
  -I api/proto \
  --python_out="$OUT_DIR" \
  --grpc_python_out="$OUT_DIR" \
  "${PROTO_FILES[@]}"

# grpcio's _pb2_grpc.py emits absolute imports like "from tipsy.config.v1
# import config_pb2 as ...". We pin the generated tree under a single
# package root so callers can `import tipsy_ab_config._proto.tipsy.config.v1.config_pb2`.
# Ensure every level has an __init__.py.
find "$OUT_DIR" -type d -exec touch {}/__init__.py \;

# Rewrite proto imports to relative ones (tipsy.X → ..tipsy.X) so the
# generated tree works after relocation under tipsy_ab_config._proto.
"$PYTHON" - "$OUT_DIR" <<'PY'
import os, re, sys
root = sys.argv[1]
# Rewrite "from tipsy." -> "from tipsy_ab_config._proto.tipsy."
# and "import tipsy." -> "from tipsy_ab_config._proto import tipsy."
import_from = re.compile(r"^from tipsy\.", re.M)
import_plain = re.compile(r"^import tipsy\.", re.M)
for dirpath, _, files in os.walk(root):
    for f in files:
        if not f.endswith(".py"):
            continue
        p = os.path.join(dirpath, f)
        with open(p, "r", encoding="utf-8") as fh:
            src = fh.read()
        new = import_from.sub("from tipsy_ab_config._proto.tipsy.", src)
        new = import_plain.sub("from tipsy_ab_config._proto import tipsy.", new)
        if new != src:
            with open(p, "w", encoding="utf-8") as fh:
                fh.write(new)
PY

echo "Generated Python proto bindings under $OUT_DIR"
for f in "${PROTO_FILES[@]}"; do
  echo "  source: $f"
done
