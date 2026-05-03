from __future__ import annotations

from dataclasses import dataclass


@dataclass
class ExecResult:
    stdout: str
    stderr: str
    exit_code: int
    timed_out: bool = False
    duration_ms: int = 0


@dataclass
class FileEntry:
    name: str
    size: int
    mod_unix: int
    is_dir: bool
