from __future__ import annotations

import base64
import json
import re
import time
from typing import Any, Mapping


def runpod_proxy_base(pod_id: str) -> str:
    """HTTPS origin for RunPod proxy URL (matches backend `RunPodProxyURL`)."""
    pid = (pod_id or "").strip()
    if not pid:
        return ""
    return f"https://{pid}-8443.proxy.runpod.net"


def derive_data_endpoint(pod_proxy_url: str | None, pod_id: str | None) -> str:
    u = (pod_proxy_url or "").strip().rstrip("/")
    if u:
        return u
    return runpod_proxy_base(pod_id or "").rstrip("/")


def strip_scheme(host_or_url: str) -> str:
    s = (host_or_url or "").strip()
    s = s.removeprefix("https://").removeprefix("http://").removeprefix("wss://").removeprefix("ws://")
    return s.rstrip("/")


def shell_url_for(data_endpoint: str, sandbox_id: str, token: str) -> str:
    host = strip_scheme(data_endpoint)
    if not host:
        return ""
    return f"wss://{host}/sandbox/{sandbox_id}/shell?token={token}"


def pick(d: Mapping[str, Any], *keys: str) -> Any:
    for k in keys:
        if k in d and d[k] is not None:
            return d[k]
    return None


_RUNPOD_HOST_RE = re.compile(
    r"^https?://(?P<pod>[a-zA-Z0-9_-]+)-8443\.proxy\.runpod\.net/?$",
)

def parse_pod_id_from_endpoint(url: str) -> str | None:
    m = _RUNPOD_HOST_RE.match((url or "").strip().rstrip("/"))
    return m.group("pod") if m else None


def json_loads_maybe(raw: str) -> dict[str, Any] | None:
    raw = raw.strip()
    if not raw:
        return None
    try:
        v = json.loads(raw)
        return v if isinstance(v, dict) else None
    except json.JSONDecodeError:
        return None


def looks_like_pod_starting(status_code: int, body_text: str, parsed: Mapping[str, Any] | None) -> bool:
    if status_code == 503:
        return True
    blob = (body_text or "").lower()
    if "pod_starting" in blob or "starting" in blob and "pod" in blob:
        return True
    if parsed:
        slug = str(parsed.get("code") or parsed.get("error") or "").lower()
        if slug == "pod_starting":
            return True
    return False


def b64encode_utf8(s: str) -> str:
    return base64.standard_b64encode(s.encode("utf-8")).decode("ascii")


def b64decode_utf8(s: str) -> str:
    return base64.standard_b64decode(s.encode("ascii")).decode("utf-8")
