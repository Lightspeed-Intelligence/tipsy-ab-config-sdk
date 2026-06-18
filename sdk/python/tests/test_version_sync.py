"""Single-source-of-truth version consistency checks.

Asserts that all three version surfaces stay in lockstep:

1. ``tipsy_ab_config.__version__`` (set in ``tipsy_ab_config/__init__.py``).
2. ``pyproject.toml`` ``project.version``.
3. A ``## [<version>]`` header in ``CHANGELOG.md``.

Also smoke-imports the shipped ``*_pb2_grpc`` stubs so that any future
breakage of the ``scripts/gen-proto-py.sh`` import-rewrite tail surfaces at
plain ``pytest -q`` time, rather than only inside the opt-in packaging-marker
test or only when a downstream caller instantiates a stub.

This module runs in the default ``pytest -q`` matrix (no marker) on every
supported Python version (3.10 / 3.11 / 3.12 / 3.13).
"""

from __future__ import annotations

import pathlib
import re
import sys

import tipsy_ab_config

# Python 3.11+ has ``tomllib`` in stdlib; 3.10 needs the ``tomli`` shim
# (declared in ``pyproject.toml`` dev extras as ``tomli; python_version < '3.11'``).
if sys.version_info >= (3, 11):
    import tomllib
else:  # pragma: no cover - exercised on the 3.10 matrix leg only
    import tomli as tomllib


# tests/ lives directly under sdk/python/, so parents[1] is the SDK project root
# that owns pyproject.toml + CHANGELOG.md.
PROJECT_ROOT = pathlib.Path(__file__).resolve().parents[1]


def test_version_sync() -> None:
    """``__version__`` ↔ ``pyproject.toml`` ↔ ``CHANGELOG.md`` must agree."""
    with open(PROJECT_ROOT / "pyproject.toml", "rb") as f:
        pyproj_ver = tomllib.load(f)["project"]["version"]

    assert tipsy_ab_config.__version__ == pyproj_ver, (
        tipsy_ab_config.__version__,
        pyproj_ver,
    )

    changelog = (PROJECT_ROOT / "CHANGELOG.md").read_text(encoding="utf-8")
    assert re.search(rf"^## \[{re.escape(pyproj_ver)}\]", changelog, re.M), (
        f"CHANGELOG.md is missing '## [{pyproj_ver}]' header"
    )


def test_grpc_stubs_import_cleanly() -> None:
    """All shipped ``*_pb2_grpc`` modules must import without error.

    Catches future regressions where ``scripts/gen-proto-py.sh``'s
    ``from tipsy.<m>.v1 import ...`` → ``from tipsy_ab_config._proto.tipsy.<m>.v1
    import ...`` rewrite tail is silently dropped: the resulting stubs would
    raise ``ModuleNotFoundError: No module named 'tipsy'`` at import time. The
    package-level ``import tipsy_ab_config`` does NOT transitively load these
    modules (``client.py`` only imports them lazily inside its own module
    body), so this test fills a gap that the existing 11 test files do not
    cover at module-import time.

    Per design DR-001, the audit proto subtree is intentionally NOT shipped by
    this SDK (it lives in the private ab-config backend repo only), so only
    config + abtest stubs are smoke-imported here.
    """
    from tipsy_ab_config._proto.tipsy.config.v1 import config_pb2_grpc as _c  # noqa: F401
    from tipsy_ab_config._proto.tipsy.abtest.v1 import abtest_pb2_grpc as _a  # noqa: F401
