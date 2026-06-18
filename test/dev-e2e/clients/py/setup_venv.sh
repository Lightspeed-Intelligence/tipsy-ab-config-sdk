#!/usr/bin/env bash
# Bootstrap a Python 3.12 venv for the ST4 Python SDK e2e driver.
#
# Two install modes (choose with $SDK_MODE):
#   - SDK_MODE=editable (default): install LOCAL sdk/python editable with the
#     [http] extra. Fast iteration in-repo. Venv: .venv.
#   - SDK_MODE=backend: install the RELEASED tipsy-ab-config==v0.3.0 via
#     git+ssh at tag python-sdk/v0.3.0 (subdirectory=sdk/python). This is the
#     true downstream consumer pattern (see sdk/python/README.md §Consumer
#     onboarding). Venv: .venv-backend.
#
# The system python is 3.14 with no grpcio/httpx wheels, so we MUST use
# /usr/bin/python3.12 (verified available on this host).
#
# Run from the repo root:
#   bash test/dev-e2e/clients/py/setup_venv.sh                       # editable
#   SDK_MODE=backend bash test/dev-e2e/clients/py/setup_venv.sh      # released

set -euo pipefail

PY312="${PYTHON312:-/usr/bin/python3.12}"
SDK_MODE="${SDK_MODE:-editable}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

case "$SDK_MODE" in
	editable)
		VENV="$HERE/.venv"
		# Resolve the SDK repo root: walk up until we find sdk/python/pyproject.toml
		# (this is the public SDK repo's root marker; there is no top-level go.mod).
		REPO_ROOT="$HERE"
		while [ "$REPO_ROOT" != "/" ] && [ ! -f "$REPO_ROOT/sdk/python/pyproject.toml" ]; do
			REPO_ROOT="$(dirname "$REPO_ROOT")"
		done
		SDK_DIR="$REPO_ROOT/sdk/python"
		;;
	backend)
		VENV="$HERE/.venv-backend"
		SDK_DIR=""  # not used
		;;
	*)
		echo "FATAL: unknown SDK_MODE=$SDK_MODE (use editable|backend)" >&2
		exit 2
		;;
esac

if [ ! -x "$PY312" ]; then
	echo "FATAL: $PY312 not found/executable. Set PYTHON312 to a python3.12 binary." >&2
	exit 2
fi
if [ "$SDK_MODE" = "editable" ] && [ ! -d "$SDK_DIR" ]; then
	echo "FATAL: SDK dir not found at $SDK_DIR" >&2
	exit 2
fi

echo "Python   : $PY312 ($("$PY312" --version 2>&1))"
echo "Mode     : $SDK_MODE"
echo "venv     : $VENV"
[ -n "$SDK_DIR" ] && echo "SDK dir  : $SDK_DIR"

# Some distros ship python3.12 WITHOUT ensurepip (the python3.12-venv package).
# In that case `python -m venv` fails to seed pip. We fall back to creating the
# venv WITHOUT pip and then bootstrapping pip into it via the SYSTEM pip's
# --python flag (no sudo / no apt needed). Verified on this host (Ubuntu,
# system pip 24+ for python3.12, ensurepip absent).
if "$PY312" -m venv "$VENV" 2>/dev/null && [ -x "$VENV/bin/python" ] && "$VENV/bin/python" -m pip --version >/dev/null 2>&1; then
	echo "venv created with bundled pip"
else
	echo "ensurepip unavailable; creating venv --without-pip and bootstrapping pip"
	rm -rf "$VENV"
	"$PY312" -m venv --without-pip "$VENV"
	# System pip installs pip INTO the venv (flag order: --python BEFORE subcommand).
	"$PY312" -m pip --python "$VENV/bin/python" install pip
fi

# shellcheck disable=SC1091
"$VENV/bin/python" -m pip install --upgrade pip

if [ "$SDK_MODE" = "editable" ]; then
	# Editable install of the local SDK with the [http] extra. gRPC deps
	# (grpcio, protobuf) come from the base dependencies.
	"$VENV/bin/python" -m pip install -e "${SDK_DIR}[http]"
else
	# True backend pattern: install the released v0.3.0 from git+ssh at the
	# tagged commit on the PUBLIC SDK repo. Auth uses the same SSH key that
	# already works for `git clone git@github.com:Lightspeed-Intelligence/tipsy-ab-config-sdk.git`.
	# The README (sdk/python/README.md) documents the equivalent git+https +
	# ${GH_PAT} form for CI environments. As the SDK repo is public, plain
	# git+https with no token also works.
	"$VENV/bin/python" -m pip install \
		'tipsy-ab-config[http] @ git+ssh://git@github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk.git@python-sdk/v0.3.0#subdirectory=sdk/python'
	"$VENV/bin/python" -c "import tipsy_ab_config as p; print('SDK version:', p.__version__)"
fi

echo
echo "Done. To run the driver (HTTP mode is solid; gRPC mode is best-effort):"
echo
echo "  export AB_CONFIG_TOKEN='<dev service token>'   # see docs/dev-http-token.md"
echo "  $VENV/bin/python $HERE/run.py                  # both transports"
echo "  $VENV/bin/python $HERE/run.py --transport http # HTTP only"
echo
echo "gRPC mode (direct origin IP) needs the Cloudflare Origin CA PEM because"
echo "grpcio has no native InsecureSkipVerify. Fetch it once and point"
echo "AB_CONFIG_GRPC_CA_PEM at it:"
echo
echo "  openssl s_client -connect 47.253.175.59:443 \\"
echo "    -servername dev-ab-config-grpc.infra.fantacy.live </dev/null 2>/dev/null \\"
echo "    | openssl x509 > $HERE/origin.pem"
echo "  export AB_CONFIG_GRPC_CA_PEM='$HERE/origin.pem'"
echo
echo "Without that PEM, gRPC mode is reported as DEGRADED (the driver continues"
echo "with HTTP mode). The raw-gRPC correctness evidence is grpc_smoke.sh."

