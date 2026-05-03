from __future__ import annotations

import json
from collections.abc import Callable
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from sockt.client import Sandbox


class ShellSession:
    """Minimal synchronous WebSocket PTY session (runtime `/sandbox/{id}/shell`)."""

    def __init__(self, sandbox: Sandbox) -> None:
        self._sb = sandbox
        self._ws: Any = None

    def __enter__(self) -> ShellSession:
        try:
            from websockets.sync.client import connect
        except ImportError as e:
            raise RuntimeError("websockets package required for shell()") from e

        assert self._sb._client is not None
        self._sb._client._ensure_data_jwt(self._sb)
        uri = self._sb.shell_url
        self._ws = connect(uri, additional_headers={"Authorization": f"Bearer {self._sb.data_jwt}"})
        self._ws.__enter__()
        return self

    def __exit__(self, *args: object) -> None:
        if self._ws is not None:
            self._ws.__exit__(*args)
            self._ws = None

    def send_raw(self, payload: dict[str, Any]) -> None:
        assert self._ws is not None
        self._ws.send(json.dumps(payload))

    def send(self, text: str) -> None:
        self.send_raw({"type": "stdin", "data": text})

    def send_line(self, text: str) -> None:
        self.send(text + "\n")

    def resize(self, cols: int, rows: int) -> None:
        self.send_raw({"type": "resize", "cols": cols, "rows": rows})

    def recv(self, timeout_secs: float | None = 10) -> dict[str, Any]:
        assert self._ws is not None
        raw = self._ws.recv(timeout=timeout_secs)
        data = json.loads(raw)
        if not isinstance(data, dict):
            raise ValueError("non-object frame")
        return data

    def close(self) -> None:
        self.__exit__(None, None, None)

    def on_billing_event(self, _cb: Callable[[dict[str, Any]], None]) -> None:
        """Hook stub — handle `billing` / `topup` frames in your own recv loop."""
