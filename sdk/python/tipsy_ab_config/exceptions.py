"""Public exception types for the Tipsy AB-config Python SDK."""

from __future__ import annotations


class AbtestContextMissing(Exception):
    """Raised by :func:`Client.get_config` when the AbtestContext is None.

    Per ``abtest-platform-sdk.md`` §4 callers MUST pass either the result of
    :func:`Client.new_abtest_context` or :func:`Client.empty_abtest_context`.
    Forgetting to attach one (typically because the FastAPI middleware was
    not wired) is treated as a programming error and surfaces loudly here.
    """


class StartupPullFailed(Exception):
    """Raised by :func:`init` when the startup ``PullAll`` cannot succeed.

    Suppressed when ``Config.startup_fail_open`` is ``True``.
    """


class SDKClosed(Exception):
    """Raised by SDK calls made after :func:`Client.close`."""


class NamespaceRequired(Exception):
    """Raised when a namespace cannot be resolved (design 04 §B.1, decision A-3).

    Mirrors the Go SDK's ``ErrNamespaceRequired``. The ns-optional dynamic
    ``get_config`` / ``get_experiment_result`` surfaces resolve an empty
    namespace argument to the project default namespace (read once at init from
    the ``PROJECT_DEFAULT_NAMESPACE`` env var, or the
    :attr:`Config.default_namespace` override). When both are empty there is no
    namespace to use and this is raised.
    """


class NamespaceNotSubscribed(Exception):
    """Raised when a resolved namespace is not in the subscribed ``namespaces``.

    Mirrors the Go SDK's ``ErrNamespaceNotSubscribed``. The SDK only consumes
    namespaces it subscribed to at init (so it has a local cache + seq tracking
    for them); an explicit or default-resolved ns outside that set is a
    caller/config error surfaced here (design 04 §B.1 validation).
    """
