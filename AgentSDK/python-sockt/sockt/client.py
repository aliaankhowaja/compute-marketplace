from __future__ import annotations

import json
import os
import time
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from typing import Any, Mapping

import httpx

from sockt.errors import (
    APIError,
    AuthenticationError,
    HostUnreachableError,
    NoCapacityError,
    SandboxNotRunningError,
    TopUpFailedError,
)
from sockt.models import ExecResult, FileEntry
from sockt.util import (
    b64decode_utf8,
    b64encode_utf8,
    derive_data_endpoint,
    json_loads_maybe,
    looks_like_pod_starting,
    pick,
    shell_url_for,
)


def _utcnow() -> datetime:
    return datetime.now(timezone.utc)


def _parse_backend_err(resp: httpx.Response) -> tuple[str, str]:
    try:
        data = resp.json()
        if isinstance(data, dict):
            slug = str(data.get("code") or data.get("error") or "")
            msg = str(data.get("message") or slug or resp.text)
            return slug, msg
    except Exception:
        pass
    return "", resp.text or resp.reason_phrase


def _slug_maps_to_exc(slug: str, msg: str, status: int) -> Exception | None:
    s = (slug or "").lower()
    if s in {"no_capacity", "nocapacity"} or "no capacity" in msg.lower():
        return NoCapacityError(msg, slug=s or None, status_code=status)
    if s in {"sandbox_not_running", "paused", "terminated"}:
        return SandboxNotRunningError(msg, slug=s or None, status_code=status)
    if s in {"unauthorized", "forbidden"}:
        return AuthenticationError(msg, slug=s or None, status_code=status)
    if (
        "topup" in s
        or "top_up" in s
        or "deposit" in s
        or s == "deposit_required"
        or s == "out_of_balance"
        or s == "deposit_mint_failed"
    ):
        return TopUpFailedError(msg, slug=s or None, status_code=status)
    return None


@dataclass
class ComputeClient:
    """HTTP client for Sockt control plane.

    REST is credits-only (API key). Lightning sandboxes are managed via MCP, where the
    sandbox-scoped bearer token (`sbx_…`) is captured automatically by the MCP proxy.
    """

    api_url: str
    api_key: str | None = None
    supabase_jwt: str | None = None
    sandbox_token: str | None = None
    timeout_secs: float = 120.0
    data_jwt_refresh_threshold_secs: int = 600
    transport: Any | None = None

    def __post_init__(self) -> None:
        self._base = self.api_url.strip().rstrip("/")
        self._timeout = httpx.Timeout(self.timeout_secs)
        self._http = httpx.Client(timeout=self._timeout, transport=self.transport)
        thr = os.environ.get("SOCKT_DATA_JWT_REFRESH_THRESHOLD_SECS")
        if thr:
            try:
                self.data_jwt_refresh_threshold_secs = int(thr)
            except ValueError:
                pass

    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> ComputeClient:
        return self

    def __exit__(self, *args: object) -> None:
        self.close()

    def _cp_headers(self) -> dict[str, str]:
        h: dict[str, str] = {"Accept": "application/json", "Content-Type": "application/json"}
        key = self.api_key or os.environ.get("SOCKT_API_KEY")
        jwt = self.supabase_jwt or os.environ.get("SOCKT_SUPABASE_JWT") or os.environ.get("SOCKT_USER_JWT")
        sbx = self.sandbox_token or os.environ.get("SOCKT_SANDBOX_TOKEN")

        if key:
            h["Authorization"] = f"Bearer {key.strip()}"
        elif sbx:
            h["Authorization"] = f"Bearer {sbx.strip()}"
        elif jwt:
            h["Authorization"] = f"Bearer {jwt.strip()}"
        else:
            raise AuthenticationError(
                "Set api_key, supabase_jwt, or sandbox_token (or SOCKT_API_KEY / SOCKT_USER_JWT / SOCKT_SANDBOX_TOKEN).",
            )
        return h

    def _request_cp(self, method: str, path: str, **kw: Any) -> httpx.Response:
        url = self._base + path
        hdr = kw.pop("headers", {})
        hdr = {**self._cp_headers(), **hdr}
        try:
            return self._http.request(method, url, headers=hdr, **kw)
        except httpx.RequestError as e:
            raise HostUnreachableError(str(e), slug="control_plane_unreachable") from e

    def _raise_for_cp(self, resp: httpx.Response) -> None:
        if resp.is_success:
            return
        slug, msg = _parse_backend_err(resp)
        mapped = _slug_maps_to_exc(slug, msg, resp.status_code)
        if mapped:
            raise mapped
        raise APIError(msg, slug=slug or None, status_code=resp.status_code)

    # --- public ---

    def create_sandbox(self, *, billing_method: str, tier: str, prepaid_sats: int | None = None, **_: Any) -> Sandbox:
        body: dict[str, Any] = {"billing_method": billing_method, "tier": tier}
        if prepaid_sats is not None:
            body["prepaid_sats"] = prepaid_sats
        resp = self._request_cp("POST", "/v1/sandbox/create", json=body)
        self._raise_for_cp(resp)
        row = resp.json()
        if not isinstance(row, dict):
            raise APIError("invalid sandbox response")
        sb = sandbox_from_row(self, row)
        sb.refresh_data_jwt(force=True)
        return sb

    def get_sandbox(self, sandbox_id: str) -> Sandbox:
        resp = self._request_cp("GET", f"/v1/sandbox/{sandbox_id}")
        self._raise_for_cp(resp)
        row = resp.json()
        if not isinstance(row, dict):
            raise APIError("invalid sandbox response")
        sb = sandbox_from_row(self, row)
        sb.refresh_data_jwt(force=True)
        return sb

    def list_sandboxes(self, **_kwargs: Any) -> list[Sandbox]:
        """Not exposed on MVP REST router; returns empty until `GET /v1/sandboxes` exists."""

        return []

    def get_pricing(self) -> list[dict[str, Any]]:
        resp = self._request_cp("GET", "/v1/pricing")
        self._raise_for_cp(resp)
        data = resp.json()
        return data if isinstance(data, list) else []

    # --- internals ---

    def _fetch_sandbox_row(self, sandbox_id: str) -> dict[str, Any]:
        resp = self._request_cp("GET", f"/v1/sandbox/{sandbox_id}")
        self._raise_for_cp(resp)
        row = resp.json()
        if not isinstance(row, dict):
            raise APIError("invalid sandbox payload")
        return row

    def _post_refresh(self, sandbox_id: str) -> tuple[str, int]:
        resp = self._request_cp("POST", f"/v1/sandbox/{sandbox_id}/refresh-data-jwt", json={})
        self._raise_for_cp(resp)
        data = resp.json()
        if not isinstance(data, dict):
            raise APIError("invalid refresh payload")
        tok = pick(data, "data_jwt", "token")
        if not isinstance(tok, str) or not tok.strip():
            raise APIError("refresh missing data_jwt")
        secs = pick(data, "expires_in_secs", "expires_in")
        try:
            exp_secs = int(secs) if secs is not None else 3600
        except (TypeError, ValueError):
            exp_secs = 3600
        return tok.strip(), exp_secs

    def _apply_refresh_to(self, sb: Sandbox, token: str, exp_secs: int) -> None:
        sb.data_jwt = token
        sb.data_jwt_expires_at = _utcnow() + timedelta(seconds=max(1, exp_secs))
        row = self._fetch_sandbox_row(sb.id)
        sb.raw = row
        sb.status = str(pick(row, "Status", "status") or sb.status)
        sb.tier = str(pick(row, "Tier", "tier") or sb.tier)
        sb.template = str(pick(row, "Template", "template") or sb.template)
        sb.runtime = str(pick(row, "Runtime", "runtime") or sb.runtime)
        sb.billing_method = str(pick(row, "BillingMethod", "billing_method") or sb.billing_method)
        sb.consumed_msats = int(pick(row, "ConsumedMsats", "consumed_msats") or 0)
        pod_id = pick(row, "PodID", "pod_id")
        sb.pod_id = str(pod_id).strip() if pod_id else sb.pod_id
        proxy = pick(row, "PodProxyURL", "pod_proxy_url")
        sb.data_endpoint = derive_data_endpoint(
            str(proxy).strip() if proxy else None,
            sb.pod_id,
        ).rstrip("/")

    def _ensure_data_jwt(self, sb: Sandbox, *, force: bool = False) -> None:
        need = force
        if sb.data_jwt_expires_at:
            remain = (sb.data_jwt_expires_at - _utcnow()).total_seconds()
            if remain < float(self.data_jwt_refresh_threshold_secs):
                need = True
        elif not sb.data_jwt:
            need = True
        if need:
            sb.refresh_data_jwt(force=True)

    def _data_request(self, sb: Sandbox, method: str, path: str, **kw: Any) -> httpx.Response:
        self._ensure_data_jwt(sb)

        def once(extra_headers: dict[str, str] | None = None) -> httpx.Response:
            headers = dict(kw.get("headers") or {})
            if extra_headers:
                headers.update(extra_headers)
            headers["Authorization"] = f"Bearer {sb.data_jwt}"
            opts = {k: v for k, v in kw.items() if k != "headers"}
            url = sb.data_endpoint.rstrip("/") + path
            try:
                return self._http.request(method, url, headers=headers, timeout=self._timeout, **opts)
            except httpx.RequestError as e:
                raise HostUnreachableError(str(e), slug="host_unreachable") from e

        resp = once()
        if resp.status_code == 401:
            body_low = (resp.text or "").lower()
            if "data_jwt_expired" in body_low or resp.status_code == 401:
                sb.refresh_data_jwt(force=True)
                resp = once()

        parsed = json_loads_maybe(resp.text or "")
        if looks_like_pod_starting(resp.status_code, resp.text or "", parsed):
            time.sleep(float(os.environ.get("SOCKT_POD_STARTING_BACKOFF_SECS", "2")))
            try:
                sb.refresh_data_jwt(force=True)
            except Exception:
                pass
            resp = once()

        if resp.status_code == 401:
            raise AuthenticationError(resp.text or "unauthorized", status_code=401)

        if resp.status_code in {503, 502, 504}:
            try:
                sb.refresh_data_jwt(force=True)
                resp2 = once()
                if resp2.status_code < 500:
                    return resp2
            except Exception:
                pass
            raise HostUnreachableError(resp.text or "data plane unreachable", slug="host_unreachable", status_code=resp.status_code)

        try:
            resp.raise_for_status()
        except httpx.HTTPStatusError as e:
            raise APIError(e.response.text or str(e), status_code=e.response.status_code) from e

        return resp


@dataclass(kw_only=True)
class Sandbox:
    id: str
    status: str
    tier: str
    template: str
    runtime: str
    billing_method: str
    pod_id: str | None
    data_endpoint: str
    data_jwt: str
    data_jwt_expires_at: datetime | None
    consumed_msats: int = 0
    sandbox_token: str | None = None
    raw: Mapping[str, Any] | None = None
    _client: ComputeClient | None = None

    @property
    def shell_url(self) -> str:
        assert self._client is not None
        self._client._ensure_data_jwt(self)
        return shell_url_for(self.data_endpoint, self.id, self.data_jwt)

    def refresh_data_jwt(self, *, force: bool = False) -> None:
        assert self._client is not None
        if not force and self.data_jwt_expires_at:
            remain = (self.data_jwt_expires_at - _utcnow()).total_seconds()
            if remain >= float(self._client.data_jwt_refresh_threshold_secs):
                return
        tok, secs = self._client._post_refresh(self.id)
        self._client._apply_refresh_to(self, tok, secs)

    def exec(
        self,
        command: str,
        *,
        timeout_ms: int = 30_000,
        working_dir: str = ".",
    ) -> ExecResult:
        assert self._client is not None
        self._require_running()
        sec = max(1, min(300, timeout_ms // 1000))
        body = {"argv": ["/bin/bash", "-lc", command], "cwd": working_dir, "timeout_seconds": sec}
        r = self._client._data_request(self, "POST", f"/sandbox/{self.id}/exec", json=body)
        data = r.json()
        if not isinstance(data, dict):
            raise APIError("invalid exec response")
        return ExecResult(
            stdout=str(data.get("stdout") or ""),
            stderr=str(data.get("stderr") or ""),
            exit_code=int(data.get("exit_code") if data.get("exit_code") is not None else -1),
            timed_out=bool(data.get("timed_out") or False),
            duration_ms=int(data.get("duration_ms") or 0),
        )

    def write_file(self, path: str, content: str, *, encoding: str = "utf8", create_dirs: bool = True) -> dict[str, Any]:
        assert self._client is not None
        self._require_running()
        enc = encoding.lower()
        b64 = content if enc == "base64" else b64encode_utf8(content)
        body = {"path": path, "mode": 0o644, "content_base64": b64, "create_dirs": create_dirs}
        r = self._client._data_request(self, "POST", f"/sandbox/{self.id}/files", json=body)
        data = r.json()
        return dict(data) if isinstance(data, dict) else {"ok": True}

    def read_file(self, path: str, *, encoding: str = "utf8", max_bytes: int = 10_485_760) -> tuple[str, dict[str, Any]]:
        assert self._client is not None
        self._require_running()
        params = {"path": path}
        r = self._client._data_request(self, "GET", f"/sandbox/{self.id}/files", params=params)
        data = r.json()
        if not isinstance(data, dict):
            raise APIError("invalid read_file response")
        raw_b64 = str(data.get("content_base64") or "")
        blob = b64decode_utf8(raw_b64)
        truncated = False
        if max_bytes > 0 and len(blob) > max_bytes:
            blob = blob[:max_bytes]
            truncated = True
        meta = {"truncated": truncated, "path": data.get("path")}
        if encoding.lower() == "base64":
            return b64encode_utf8(blob), meta
        return blob, meta

    def list_files(self, path: str = ".", **_ignored: Any) -> list[FileEntry]:
        """Lists a single directory (`dir` query). Recursive listing is not supported by runtime MVP."""

        assert self._client is not None
        self._require_running()
        params = {"dir": path}
        r = self._client._data_request(self, "GET", f"/sandbox/{self.id}/files/list", params=params)
        data = r.json()
        files = data.get("files") if isinstance(data, dict) else None
        out: list[FileEntry] = []
        if isinstance(files, list):
            for fe in files:
                if not isinstance(fe, dict):
                    continue
                out.append(
                    FileEntry(
                        name=str(fe.get("name") or ""),
                        size=int(fe.get("size") or 0),
                        mod_unix=int(fe.get("mod_unix") or 0),
                        is_dir=bool(fe.get("is_dir")),
                    )
                )
        return out

    def runtime_status(self) -> dict[str, Any]:
        assert self._client is not None
        self._require_running()
        r = self._client._data_request(self, "GET", f"/sandbox/{self.id}/status")
        data = r.json()
        return dict(data) if isinstance(data, dict) else {}

    def pause(self) -> None:
        assert self._client is not None
        resp = self._client._request_cp("POST", f"/v1/sandbox/{self.id}/pause")
        self._client._raise_for_cp(resp)
        self.status = "paused"

    def resume(self) -> None:
        assert self._client is not None
        resp = self._client._request_cp("POST", f"/v1/sandbox/{self.id}/resume")
        self._client._raise_for_cp(resp)
        self.status = "running"
        self.refresh_data_jwt(force=True)

    def terminate(self, *, lightning_address: str | None = None) -> None:
        assert self._client is not None
        body: dict[str, Any] = {}
        if lightning_address:
            body["lightning_address"] = lightning_address
        resp = self._client._request_cp("DELETE", f"/v1/sandbox/{self.id}", json=body if body else None)
        self._client._raise_for_cp(resp)

    def extend_balance(self, *, cents: int | None = None) -> dict[str, Any]:
        """Allocate more credits for this sandbox (credits-only). Lightning sandboxes top up by paying the
        BOLT11 surfaced in `sandbox_status.pending_invoice` via MCP — no REST call required."""

        assert self._client is not None
        body: dict[str, Any] = {}
        if cents is not None:
            body["cents"] = cents
        resp = self._client._request_cp("POST", f"/v1/sandbox/{self.id}/extend-balance", json=body)
        self._client._raise_for_cp(resp)
        data = resp.json()
        return dict(data) if isinstance(data, dict) else {}

    def wait_until_running(self, *, timeout_secs: float = 120, poll_interval_secs: float = 2) -> None:
        assert self._client is not None
        deadline = time.time() + timeout_secs
        while time.time() < deadline:
            row = self._client._fetch_sandbox_row(self.id)
            st = str(pick(row, "Status", "status") or "").lower()
            self.status = st
            if st == "running":
                return
            time.sleep(poll_interval_secs)
        raise SandboxNotRunningError(f"sandbox {self.id} not running within {timeout_secs}s")

    def shell(self):
        from sockt.shell import ShellSession

        return ShellSession(self)

    def _require_running(self) -> None:
        st = (self.status or "").lower()
        if st in {"paused", "terminated", "stopped"}:
            raise SandboxNotRunningError(f"sandbox status is {self.status}")


def sandbox_from_row(client: ComputeClient, row: Mapping[str, Any]) -> Sandbox:
    sid = pick(row, "ID", "id")
    if not sid:
        raise APIError("sandbox id missing from API row")
    sandbox_uuid = str(sid)
    pod_id = pick(row, "PodID", "pod_id")
    pod_s = str(pod_id).strip() if pod_id else None
    proxy = pick(row, "PodProxyURL", "pod_proxy_url")
    endpoint = derive_data_endpoint(str(proxy).strip() if proxy else None, pod_s)

    sbx_tok = pick(row, "SandboxToken", "sandbox_token")
    return Sandbox(
        id=sandbox_uuid,
        status=str(pick(row, "Status", "status") or ""),
        tier=str(pick(row, "Tier", "tier") or ""),
        template=str(pick(row, "Template", "template") or ""),
        runtime=str(pick(row, "Runtime", "runtime") or ""),
        billing_method=str(pick(row, "BillingMethod", "billing_method") or ""),
        pod_id=pod_s,
        data_endpoint=endpoint.rstrip("/"),
        data_jwt="",
        data_jwt_expires_at=None,
        consumed_msats=int(pick(row, "ConsumedMsats", "consumed_msats") or 0),
        sandbox_token=str(sbx_tok).strip() if sbx_tok else None,
        raw=dict(row),
        _client=client,
    )
