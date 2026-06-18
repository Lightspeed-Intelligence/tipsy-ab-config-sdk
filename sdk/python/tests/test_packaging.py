"""Wheel content acceptance checks (``@pytest.mark.packaging``, opt-in).

This test is *not* collected by default ``pytest -q`` — ``pyproject.toml``'s
``[tool.pytest.ini_options].addopts = "-m 'not packaging'"`` filters it out.
The CI ``build`` job runs it explicitly via:

    pytest -m packaging --override-ini="addopts=" -q

The ``--override-ini`` clears the default ``addopts`` so the ``-m packaging``
selector is unambiguous regardless of future pytest precedence drift.

Assertions cover design §Step 4 + §Acceptance #3 + Round 3 patch's import
rewrite check + coding-agent's iteration-2 METADATA cross-check:

1. White-list — required files present in the wheel.
2. Black-list — repository hygiene paths (``tests/``, ``.venv/``,
   ``.pytest_cache/``, ``__pycache__/``, ``*.egg-info/``, ``example/``)
   must not leak into the wheel.
3. Import rewrite — every ``_pb2_grpc.py`` inside the wheel must have its
   cross-stub imports rewritten to ``from tipsy_ab_config._proto.tipsy.<m>.v1
   import ...``. No bare ``from tipsy.`` or ``import tipsy.`` may survive.
4. Wheel ``METADATA`` ``Version:`` line must equal
   ``tipsy_ab_config.__version__``.

Runtime smoke (clean-venv ``pip install`` + ``import``) is intentionally NOT
duplicated here — it's covered by the CI build job's dedicated step.
"""

from __future__ import annotations

import glob
import pathlib
import re
import subprocess
import sys
import zipfile

import pytest

import tipsy_ab_config


# tests/ sits directly under sdk/python/, so parents[1] is the project root
# (the directory that holds pyproject.toml).
PROJECT_ROOT = pathlib.Path(__file__).resolve().parents[1]


pytestmark = pytest.mark.packaging


@pytest.fixture(scope="module")
def built_wheel(tmp_path_factory: pytest.TempPathFactory) -> pathlib.Path:
    """Build a fresh wheel into a module-scoped temp dir and return its path.

    We always build on the fly rather than reuse ``sdk/python/dist/`` — this
    guarantees the test reflects the *current* source tree, not a stale
    artefact left over from a previous local run.
    """
    out_dir = tmp_path_factory.mktemp("wheel")
    subprocess.run(
        [sys.executable, "-m", "build", "--wheel", "--outdir", str(out_dir)],
        cwd=str(PROJECT_ROOT),
        check=True,
    )
    wheels = glob.glob(str(out_dir / "*.whl"))
    assert len(wheels) == 1, f"expected exactly one built wheel, found: {wheels}"
    return pathlib.Path(wheels[0])


def _names(wheel_path: pathlib.Path) -> list[str]:
    with zipfile.ZipFile(wheel_path) as zf:
        return zf.namelist()


def test_wheel_whitelist(built_wheel: pathlib.Path) -> None:
    """Files that MUST be inside the wheel."""
    names = _names(built_wheel)

    # Required individual files.
    assert "tipsy_ab_config/__init__.py" in names
    assert "tipsy_ab_config/py.typed" in names

    # All proto bindings must be shipped — at minimum, each of the two
    # versioned packages (config/v1, abtest/v1) must contribute at least one
    # ``_pb2.py`` AND one ``_pb2_grpc.py``. Per design DR-001, the audit subtree
    # is intentionally NOT shipped (it lives in the private ab-config backend
    # repo only).
    for pkg in ("config", "abtest"):
        prefix = f"tipsy_ab_config/_proto/tipsy/{pkg}/v1/"
        pb2 = [n for n in names if n.startswith(prefix) and n.endswith("_pb2.py")]
        grpc = [n for n in names if n.startswith(prefix) and n.endswith("_pb2_grpc.py")]
        assert pb2, f"wheel is missing any *_pb2.py under {prefix}; have: {names}"
        assert grpc, f"wheel is missing any *_pb2_grpc.py under {prefix}; have: {names}"

    # Every level of the ``_proto`` tree must carry an ``__init__.py`` so the
    # package is importable post-install (mirrors the regen script's idempotent
    # ``touch`` tail).
    for required_init in (
        "tipsy_ab_config/_proto/__init__.py",
        "tipsy_ab_config/_proto/tipsy/__init__.py",
        "tipsy_ab_config/_proto/tipsy/config/__init__.py",
        "tipsy_ab_config/_proto/tipsy/config/v1/__init__.py",
        "tipsy_ab_config/_proto/tipsy/abtest/__init__.py",
        "tipsy_ab_config/_proto/tipsy/abtest/v1/__init__.py",
    ):
        assert required_init in names, f"wheel is missing {required_init}"


def test_wheel_blacklist(built_wheel: pathlib.Path) -> None:
    """Paths that MUST NOT leak into the wheel."""
    names = _names(built_wheel)

    # Anything under ``tests/`` is a hard fail — packages.find already excludes
    # it, but a regression in ``[tool.setuptools.packages.find].include`` would
    # silently re-include it.
    leaked_tests = [n for n in names if n.startswith("tests/")]
    assert not leaked_tests, f"wheel leaked test files: {leaked_tests}"

    # Forbidden path fragments anywhere in the archive.
    forbidden_fragments = (
        ".venv/",
        ".pytest_cache/",
        "__pycache__/",
        "tipsy_ab_config.egg-info/",
        "example/",
    )
    for fragment in forbidden_fragments:
        leaked = [n for n in names if fragment in n]
        assert not leaked, f"wheel leaked '{fragment}': {leaked}"


def test_wheel_grpc_stubs_have_rewritten_imports(built_wheel: pathlib.Path) -> None:
    """All ``_pb2_grpc.py`` files inside the wheel must use rewritten imports.

    The default protoc output emits ``from tipsy.<m>.v1 import <m>_pb2 as ...``
    which fails at runtime in the wheel because ``tipsy`` is not a top-level
    package — only ``tipsy_ab_config._proto.tipsy`` is. The existing
    ``scripts/gen-proto-py.sh`` runs a Python regex pass that rewrites those
    bare imports. This test guards against a future commit silently dropping
    that rewrite tail.

    Regex semantics (multi-line mode):
      * ``^from tipsy\\.`` — bare ``from tipsy.config.v1 import ...`` (the
        protoc default; what we must NEVER see in the shipped wheel).
      * ``^import tipsy\\.`` — same for the ``import`` flavour.
    """
    bare_from = re.compile(r"^from tipsy\.", re.M)
    bare_import = re.compile(r"^import tipsy\.", re.M)

    with zipfile.ZipFile(built_wheel) as zf:
        grpc_files = [n for n in zf.namelist() if n.endswith("_pb2_grpc.py")]
        assert grpc_files, "wheel ships no *_pb2_grpc.py at all — sanity check"

        for name in grpc_files:
            src = zf.read(name).decode("utf-8")
            assert not bare_from.search(src), (
                f"{name} contains an un-rewritten bare 'from tipsy.' import — "
                f"scripts/gen-proto-py.sh's rewrite tail was bypassed or "
                f"removed; the shipped wheel will ModuleNotFoundError at "
                f"runtime."
            )
            assert not bare_import.search(src), (
                f"{name} contains an un-rewritten bare 'import tipsy.' line — "
                f"see above."
            )


def test_wheel_metadata_version_matches_package(built_wheel: pathlib.Path) -> None:
    """``METADATA``'s ``Version:`` line must equal ``tipsy_ab_config.__version__``.

    Iteration-2 discovered test point: catches a build-time mismatch where
    ``pyproject.toml`` was bumped but ``__init__.py.__version__`` was not (or
    vice-versa), in the narrow window where setuptools succeeds in emitting a
    wheel anyway. ``test_version_sync.py`` already catches the source-tree
    case; this catches the same thing one level deeper, inside the actual
    built artefact.
    """
    # The METADATA path is keyed by the (normalised) project name + version,
    # e.g. ``tipsy_ab_config-0.1.0.dist-info/METADATA``. Glob it from namelist
    # rather than hard-coding so a future version bump does not require a test
    # edit.
    with zipfile.ZipFile(built_wheel) as zf:
        metadata_candidates = [
            n
            for n in zf.namelist()
            if n.startswith("tipsy_ab_config-") and n.endswith(".dist-info/METADATA")
        ]
        assert len(metadata_candidates) == 1, (
            f"expected exactly one METADATA file, got: {metadata_candidates}"
        )
        metadata = zf.read(metadata_candidates[0]).decode("utf-8")

    m = re.search(r"^Version: (.+)$", metadata, re.M)
    assert m, f"METADATA has no 'Version:' line; got:\n{metadata[:500]}"
    wheel_version = m.group(1).strip()
    assert wheel_version == tipsy_ab_config.__version__, (
        f"wheel METADATA Version={wheel_version!r} != "
        f"tipsy_ab_config.__version__={tipsy_ab_config.__version__!r}"
    )
