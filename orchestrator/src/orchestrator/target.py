"""`target.yaml` loader + live-reload watcher.

Defines the mainnet-composition target, byte budget, and QP verb set. The shape
is no longer pinned by ``chain_identity_hash`` — it is a per-batch input that
the controller can adopt across batches via ``LiveTargetWatcher``.

Every YAML field is required — the orchestrator refuses to run on a silent default
because silent defaults hide configuration drift across runs. An operator who's
serious about the reproducibility contract (every batch's ``target_sha256``
points at a snapshot in ``manifest.target_history``) should have to state each
choice explicitly.
"""

from __future__ import annotations

import hashlib
import logging
import re
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import yaml

_log = logging.getLogger(__name__)

_BASE_ADDRESS_RE = re.compile(r"^0x[0-9a-fA-F]{1,40}$")


@dataclass(frozen=True)
class TargetConfig:
    mainnet_target: dict[str, float]
    target_total_bytes: int
    base_address: bytes
    revision: int
    qp_scenarios: tuple[str, ...]
    total_batch_bytes: int
    projection_eta: float
    raw: dict[str, Any]
    source_sha256: str
    reference_f_path: Path | None = None

    @property
    def sha256(self) -> str:
        """Per-batch identity of this target shape — surfaced as a stable alias."""
        return self.source_sha256

    def byte_target(self, axis: str) -> float:
        return self.mainnet_target[axis] * self.target_total_bytes


def load_target(path: Path | str) -> TargetConfig:
    path = Path(path)
    raw_bytes = path.read_bytes()
    return parse_target(raw_bytes)


def parse_target(raw_bytes: bytes) -> TargetConfig:
    """Parse + validate raw YAML bytes into a ``TargetConfig``.

    Hashing and parsing both consume the same byte buffer, so a partial mid-edit
    cannot produce a config whose ``source_sha256`` doesn't match its body. The
    live watcher relies on this atomicity: it reads bytes once, hashes once,
    parses once.
    """
    body = yaml.safe_load(raw_bytes) or {}

    mainnet = _require(body, "mainnet_target", dict)
    _validate_mainnet_target(mainnet)
    base_addr = _parse_base_address(_require(body, "base_address", str))
    revision = int(_require(body, "revision", int))
    if revision < 0 or revision >= (1 << 80):
        raise ValueError(f"revision out of range: {revision}")

    qp_scenarios_raw = _require(body, "qp_scenarios", list)
    if not qp_scenarios_raw:
        raise ValueError("qp_scenarios must be a non-empty list")

    return TargetConfig(
        mainnet_target={k: float(v) for k, v in mainnet.items()},
        target_total_bytes=int(_require(body, "target_total_bytes", int)),
        base_address=base_addr,
        revision=revision,
        qp_scenarios=tuple(str(v) for v in qp_scenarios_raw),
        total_batch_bytes=int(_require(body, "total_batch_bytes", int)),
        projection_eta=float(_require(body, "projection_eta", (int, float))),
        reference_f_path=(
            Path(body["reference_f_path"]) if body.get("reference_f_path") is not None else None
        ),
        raw=body,
        source_sha256=hashlib.sha256(raw_bytes).hexdigest(),
    )


class LiveTargetWatcher:
    """File-watcher that re-reads ``target.yaml`` when its mtime changes.

    Holds the absolute path + the current ``TargetConfig``. ``current()`` is
    called once per batch; if the on-disk mtime differs from the cached one,
    we read the bytes atomically (one ``read_bytes`` call), hash them, parse
    them, and atomically swap the cached config. On parse failure we log and
    keep the previous config — operators editing target.yaml mid-run shouldn't
    be able to crash the orchestrator with a typo.

    All access is serialized by an internal lock so the QP loop and any
    diagnostic / debug callers can read ``current()`` concurrently without
    seeing torn state.
    """

    def __init__(self, path: Path | str, *, initial: TargetConfig) -> None:
        self._path = Path(path)
        self._current = initial
        self._lock = threading.Lock()
        try:
            self._mtime_ns = self._path.stat().st_mtime_ns
        except FileNotFoundError:
            # Memory-only watcher (tests / probes that pass an in-memory config).
            self._mtime_ns = 0

    @classmethod
    def from_path(cls, path: Path | str) -> LiveTargetWatcher:
        """Build a watcher seeded from ``load_target(path)``."""
        path = Path(path)
        return cls(path, initial=load_target(path))

    @property
    def path(self) -> Path:
        return self._path

    def current(self) -> TargetConfig:
        """Return the freshest valid ``TargetConfig``.

        Stats the file once. If the mtime hasn't moved we return the cached
        config without re-reading the bytes (the QP loop's natural cadence
        means stat-per-batch is fine; we don't poll faster).
        """
        with self._lock:
            try:
                mtime_ns = self._path.stat().st_mtime_ns
            except FileNotFoundError:
                # File vanished — keep using the last good config; an operator
                # would notice via the missing-file log line and the cached
                # sha256 staying constant in the journal.
                _log.warning("target.yaml missing at %s; keeping cached config", self._path)
                return self._current
            if mtime_ns == self._mtime_ns:
                return self._current
            try:
                raw = self._path.read_bytes()
                new_cfg = parse_target(raw)
            except (OSError, ValueError, yaml.YAMLError) as exc:
                _log.warning(
                    "target.yaml reload failed (%s); keeping previous config sha=%s",
                    exc,
                    self._current.source_sha256[:8],
                )
                # Update mtime cache so we don't spam the log every batch on a
                # persistently-broken file. The operator must restore validity
                # to trigger another reload attempt.
                self._mtime_ns = mtime_ns
                return self._current
            self._mtime_ns = mtime_ns
            self._current = new_cfg
            return new_cfg


def _require(body: dict[str, Any], key: str, expected: type | tuple[type, ...]) -> Any:
    """Pull ``key`` from ``body`` or raise ``ValueError``. Enforces the top-level YAML type.

    Missing-vs-wrong-type distinction matters: ``revision: "zero"`` and a missing
    ``revision:`` field fail for different reasons and the operator should see which.
    """
    if key not in body:
        raise ValueError(f"target.yaml missing required field: {key!r}")
    value = body[key]
    if not isinstance(value, expected):
        expected_name = (
            expected.__name__
            if isinstance(expected, type)
            else "/".join(t.__name__ for t in expected)
        )
        raise ValueError(
            f"target.yaml field {key!r} must be {expected_name}, got {type(value).__name__}"
        )
    return value


def _parse_base_address(value: Any) -> bytes:
    """Parse ``0x``-prefixed hex into a 20-byte big-endian address. Rejects oversize input."""
    if not isinstance(value, str) or not _BASE_ADDRESS_RE.fullmatch(value):
        raise ValueError(f"base_address must match ^0x[0-9a-fA-F]{{1,40}}$, got {value!r}")
    try:
        raw = bytes.fromhex(value.removeprefix("0x"))
    except ValueError as exc:
        raise ValueError(f"base_address hex parse failed: {exc}") from exc
    return raw.rjust(20, b"\x00")


def _validate_mainnet_target(mainnet: dict[str, float]) -> None:
    required = {"accounts", "storage", "code"}
    missing = required - set(mainnet.keys())
    if missing:
        raise ValueError(f"mainnet_target missing axes: {sorted(missing)}")
    total = sum(float(mainnet[a]) for a in required)
    # Spec §B.3 fractions (0.141/0.817/0.043) sum to 1.001 due to rounding in the
    # Paradigm 2024 report; accept any rounding noise up to 1 pp. Larger skews are
    # genuine misconfiguration and still raise.
    if abs(total - 1.0) > 0.01:
        raise ValueError(f"mainnet_target axes must sum to 1.0 (got {total:.6f})")
