from __future__ import annotations


class SocktError(Exception):
    """Base SDK error."""

    def __init__(self, message: str = "", *, slug: str | None = None, status_code: int | None = None) -> None:
        super().__init__(message)
        self.slug = slug
        self.status_code = status_code


class AuthenticationError(SocktError):
    """JWT / API key / L402 authentication failure."""


class SandboxNotRunningError(SocktError):
    """Operation requires a running sandbox."""


class TopUpFailedError(SocktError):
    """Manual top-up / extend_balance failed."""


class NoCapacityError(SocktError):
    """Orchestrator cannot place workload (RunPod capacity)."""


class HostUnreachableError(SocktError):
    """Data plane unreachable after refresh + retry (compat name for pod proxy failures)."""


class APIError(SocktError):
    """Structured control-plane or protocol error."""
