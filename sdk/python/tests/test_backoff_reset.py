"""Unit tests for the ``_reset_backoff_if_stable`` pure helper (ST4).

Mirrors the Go SDK's ST3 backoff-reset rule ported to the Python
``_run_subscribe`` loop: a Subscribe connection that stayed alive for at
least ``threshold_s`` before dropping resets the reconnect backoff to its
initial value (1.0s); a short-lived connection keeps the current backoff so
the caller can keep escalating exponentially.

Contract under test (module-level pure fn in ``tipsy_ab_config.client``):

    _reset_backoff_if_stable(backoff, uptime_s, threshold_s) ->
        1.0        if threshold_s > 0 and uptime_s >= threshold_s
        backoff    otherwise   (returned verbatim, no mutation)

The ``threshold_s <= 0`` guard (S5, matches Go SG-a) means the reset is
NEVER taken for a non-positive threshold, no matter how large uptime is —
this is the defensive "never judge stable" switch.
"""

from __future__ import annotations

import pytest

from tipsy_ab_config.client import _reset_backoff_if_stable


# --------------------------------------------------------------------------
# Reset path: threshold_s > 0 AND uptime_s >= threshold_s  ->  1.0
# --------------------------------------------------------------------------
RESET_CASES = [
    # (backoff, uptime_s, threshold_s)  -- expected result is always 1.0
    pytest.param(2.0, 70.0, 60.0, id="uptime-above-threshold"),
    pytest.param(8.0, 70.0, 60.0, id="reset-from-8s"),
    pytest.param(30.0, 100.0, 60.0, id="reset-from-max-30s"),
    pytest.param(16.0, 60.0, 60.0, id="boundary-uptime-eq-threshold"),
    pytest.param(8.0, 0.05, 0.05, id="boundary-tiny-threshold-eq"),
    pytest.param(4.0, 0.06, 0.05, id="tiny-threshold-slightly-over"),
    pytest.param(30.0, 3.6e3, 60.0, id="uptime-far-above"),
]


@pytest.mark.parametrize("backoff, uptime_s, threshold_s", RESET_CASES)
def test_reset_to_initial_when_stable(backoff, uptime_s, threshold_s):
    got = _reset_backoff_if_stable(backoff, uptime_s, threshold_s)
    assert got == 1.0
    # Reset must yield the exact initial float, regardless of prior backoff
    # (covers the design's explicit "reset from 8.0 / 30.0" requirement).


# --------------------------------------------------------------------------
# Keep path: threshold_s > 0 BUT uptime_s < threshold_s  ->  backoff verbatim
# --------------------------------------------------------------------------
KEEP_CASES = [
    # (backoff, uptime_s, threshold_s)  -- expected result is the input backoff
    pytest.param(2.0, 59.999, 60.0, id="just-below-threshold"),
    pytest.param(8.0, 10.0, 60.0, id="short-lived-mid-backoff"),
    pytest.param(1.0, 0.0, 60.0, id="zero-uptime-keeps-initial"),
    pytest.param(30.0, 59.0, 60.0, id="short-lived-at-max-backoff"),
    pytest.param(4.0, 0.049, 0.05, id="tiny-threshold-just-under"),
]


@pytest.mark.parametrize("backoff, uptime_s, threshold_s", KEEP_CASES)
def test_keep_backoff_when_short_lived(backoff, uptime_s, threshold_s):
    got = _reset_backoff_if_stable(backoff, uptime_s, threshold_s)
    assert got == backoff
    # Short-lived connection: backoff is returned unchanged so the caller
    # keeps escalating exponentially.


# --------------------------------------------------------------------------
# Guard path: threshold_s <= 0  ->  NEVER reset, even for enormous uptime
# (S5 / Go SG-a "never judge stable")
# --------------------------------------------------------------------------
NEVER_RESET_CASES = [
    # (backoff, uptime_s, threshold_s)  -- expected result is the input backoff
    pytest.param(8.0, 1e9, 0.0, id="threshold-zero-huge-uptime"),
    pytest.param(8.0, 1e9, -1.0, id="threshold-negative-huge-uptime"),
    pytest.param(30.0, 1e18, -60.0, id="threshold-neg-astronomical-uptime"),
    pytest.param(2.0, 0.0, 0.0, id="threshold-zero-zero-uptime"),
    pytest.param(1.0, 0.05, 0.0, id="threshold-zero-uptime-eq-would-be-boundary"),
]


@pytest.mark.parametrize("backoff, uptime_s, threshold_s", NEVER_RESET_CASES)
def test_never_reset_when_threshold_non_positive(backoff, uptime_s, threshold_s):
    got = _reset_backoff_if_stable(backoff, uptime_s, threshold_s)
    assert got == backoff
    # threshold_s <= 0 disables the reset entirely; uptime is irrelevant.


def test_function_is_pure_no_side_effects():
    """Same inputs must return the same output on repeated calls."""
    assert _reset_backoff_if_stable(8.0, 70.0, 60.0) == 1.0
    assert _reset_backoff_if_stable(8.0, 70.0, 60.0) == 1.0
    assert _reset_backoff_if_stable(8.0, 10.0, 60.0) == 8.0
    assert _reset_backoff_if_stable(8.0, 10.0, 60.0) == 8.0
