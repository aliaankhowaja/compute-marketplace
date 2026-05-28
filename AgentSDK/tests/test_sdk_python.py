from __future__ import annotations

import json

import httpx

from sockt.client import ComputeClient, Sandbox
from sockt.util import derive_data_endpoint, proxy_base


def test_proxy_base_shape():
    assert proxy_base("abc") == "https://abc-8443.proxy.runpod.net"


def test_derive_data_endpoint_prefers_proxy_url():
    assert derive_data_endpoint("https://x.example", "pod") == "https://x.example"


def test_refresh_updates_endpoint_after_pod_change():
    sb_uuid = "550e8400-e29b-41d4-a716-446655440000"

    def handler(request: httpx.Request) -> httpx.Response:
        path = request.url.path
        if path == f"/v1/sandbox/{sb_uuid}/refresh-data-jwt":
            return httpx.Response(
                200,
                json={"data_jwt": "jwt-one", "expires_in_secs": 600},
            )
        if path == f"/v1/sandbox/{sb_uuid}":
            body = {
                "ID": sb_uuid,
                "Status": "running",
                "Tier": "nano",
                "Template": "sockt-cpu",
                "Runtime": "cpu",
                "BillingMethod": "credits",
                "PodID": "newpod",
                "PodProxyURL": None,
                "ConsumedMsats": 0,
            }
            return httpx.Response(200, json=body)
        if path == f"/sandbox/{sb_uuid}/exec":
            assert request.headers.get("authorization") == "Bearer jwt-one"
            payload = json.loads(request.content.decode())
            assert payload["argv"][1] == "-lc"
            return httpx.Response(200, json={"stdout": "ok", "stderr": "", "exit_code": 0})
        return httpx.Response(404, text="unexpected " + path)

    transport = httpx.MockTransport(handler)
    cp = ComputeClient(
        api_url="https://cp.example",
        api_key="sockt_live_test",
        transport=transport,
    )

    row = {
        "ID": sb_uuid,
        "Status": "running",
        "Tier": "nano",
        "Template": "sockt-cpu",
        "Runtime": "cpu",
        "BillingMethod": "credits",
        "PodID": "newpod",
        "ConsumedMsats": 0,
    }
    sb = Sandbox(
        id=sb_uuid,
        status="running",
        tier="nano",
        template="sockt-cpu",
        runtime="cpu",
        billing_method="credits",
        pod_id="newpod",
        data_endpoint=derive_data_endpoint(None, "newpod"),
        data_jwt="",
        data_jwt_expires_at=None,
        consumed_msats=0,
        raw=row,
        _client=cp,
    )
    sb.refresh_data_jwt(force=True)
    assert sb.data_jwt == "jwt-one"
    res = sb.exec("echo hi")
    assert res.stdout == "ok"
    cp.close()
