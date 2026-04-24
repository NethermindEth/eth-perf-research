"""Run lifecycle: auto-detect startup, main loop, manifest writer.

Entry point is `run(target, state_dir, rpc_url, ...)`. The lifecycle is signal-safe:
SIGINT/SIGTERM flip a `stop` flag that is checked between batches; the current batch
always finishes cleanly to avoid partial journal writes.
"""
from __future__ import annotations

import enum
import signal
import time
import uuid
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable, Optional

from .controller import (
    BatchPlan,
    Controller,
    ControllerInstability,
    ControllerState,
    init_state,
)
from .facade import FacadeContext, dispatch
from .journal import (
    JournalReader,
    JournalWriter,
    Observability,
    Record,
    ReplayCore,
)
from .manifest import (
    EnvInfo,
    Manifest,
    Session,
    compute_composition_hash,
    compute_journal_sha256,
)
from .payloads import ExecutionPayloadV3, PayloadStreamWriter
from .probe import ProbeExecutor, run_probe, seed_state_from_probe
from .reference_f import ReferenceF, load_reference_f, default_reference_f_path
from .rpc import RpcClient
from .sensor import SensorClient, SensorWaitTimeout, StateObservation
from .target import TargetConfig


JOURNAL_FILENAME = "orchestrator.journal.jsonl"
PAYLOAD_FILENAME = "payloads.rlp"
MANIFEST_FILENAME = "run-manifest.json"


class StartupMode(enum.Enum):
    FRESH = "fresh"
    RESUME = "resume"


class ResumeRefused(Exception):
    """Pre-flight verification failed; operator must archive/rename the journal."""


@dataclass
class StartupDecision:
    mode: StartupMode
    last_record: Record | None
    reason: str


def resolve_startup_mode(
    state_dir: Path,
    composition_hash: str,
    head_block: int | None,
) -> StartupDecision:
    """Pre-flight check for fresh vs resume. Raises `ResumeRefused` on mismatch."""
    journal = state_dir / JOURNAL_FILENAME
    if not journal.exists() or journal.stat().st_size == 0:
        return StartupDecision(StartupMode.FRESH, None, "no journal at state dir")

    manifest_path = state_dir / MANIFEST_FILENAME
    if manifest_path.exists():
        prior = Manifest.read(manifest_path)
        if prior.composition_hash != composition_hash:
            raise ResumeRefused(
                f"composition_hash mismatch: manifest={prior.composition_hash[:8]}… "
                f"current={composition_hash[:8]}…"
            )

    reader = JournalReader(journal)
    reader.verify_chain()  # raises on tamper
    tail = reader.tail()
    if tail is None:
        return StartupDecision(StartupMode.FRESH, None, "journal empty after verify")

    if head_block is not None:
        if head_block not in (tail.replay_core.block_number, tail.replay_core.block_number + 1):
            raise ResumeRefused(
                f"Nethermind head {head_block} not in "
                f"{{{tail.replay_core.block_number}, {tail.replay_core.block_number + 1}}}"
            )

    return StartupDecision(StartupMode.RESUME, tail, "valid journal + head aligned")


def build_facade_context(target: TargetConfig) -> FacadeContext:
    return FacadeContext(
        base_address=target.base_address,
        revision=target.revision,
        chain_id=int(target.raw.get("chain_id", 1337)),
        gas_limit=int(target.raw.get("gas_limit", 30_000_000)),
    )


@dataclass
class LifecycleDeps:
    """Injectable dependencies so tests can stub RPC/sensor without patching."""

    sensor: SensorClient
    rpc: RpcClient
    probe_executor: ProbeExecutor | None = None


def run(
    target: TargetConfig,
    state_dir: Path | str,
    *,
    rpc_url: str,
    reference_f: ReferenceF | None = None,
    env: EnvInfo,
    max_batches: int | None = None,
    deps: LifecycleDeps | None = None,
) -> Path:
    """Main entry. Returns the path to the manifest written on shutdown."""
    state_dir = Path(state_dir)
    state_dir.mkdir(parents=True, exist_ok=True)

    target_sha = target.source_sha256
    composition_hash = compute_composition_hash(target_sha, env)
    ref_f = reference_f or load_reference_f(default_reference_f_path())

    own_deps = deps is None
    sensor = deps.sensor if deps else SensorClient(rpc_url)
    rpc = deps.rpc if deps else RpcClient(rpc_url)
    probe_exec = deps.probe_executor if deps else None

    try:
        head = rpc.eth_get_block_by_number("latest").get("number")
        head_block = int(head, 16) if isinstance(head, str) else head
    except Exception:
        head_block = None

    decision = resolve_startup_mode(state_dir, composition_hash, head_block)

    ctx = build_facade_context(target)
    if decision.mode == StartupMode.RESUME and decision.last_record is not None:
        session_id = _extract_last_session_id(state_dir) + 1
        resumed_from_batch = decision.last_record.batch_id
        # TODO: proper F reconstruction from journal tail would read observability.coeffs_after.
        state = init_state(ref_f, target.qp_scenarios)
        batch_id = decision.last_record.batch_id + 1
        ctx.address_cursor = int(decision.last_record.replay_core.end_address, 16)
    else:
        session_id = 1
        resumed_from_batch = None
        state = init_state(ref_f, target.qp_scenarios)
        batch_id = 0
        if probe_exec is not None:
            results = run_probe(ref_f, target.qp_scenarios, probe_exec)
            seed_state_from_probe(state, results)
            batch_id = len(results)

    controller = Controller(state)

    journal_path = state_dir / JOURNAL_FILENAME
    payload_path = state_dir / PAYLOAD_FILENAME
    manifest_path = state_dir / MANIFEST_FILENAME

    stop = _install_signal_handlers()
    session_started = _now_iso()
    last_observation = sensor.read()
    stop_reason = "max_batches"

    try:
        with JournalWriter(journal_path) as jw, PayloadStreamWriter(payload_path) as pw:
            completed = 0
            while not stop.requested:
                if max_batches is not None and completed >= max_batches:
                    break
                batch_status, last_observation = _run_one_batch(
                    controller=controller,
                    target=target,
                    sensor=sensor,
                    rpc=rpc,
                    facade_ctx=ctx,
                    journal_writer=jw,
                    payload_writer=pw,
                    session_id=session_id,
                    resumed_from_batch=resumed_from_batch,
                    batch_id=batch_id,
                    pre_observation=last_observation,
                )
                batch_id += 1
                completed += 1
                if batch_status != "ok":
                    stop_reason = batch_status
                    break
            else:
                stop_reason = "signal"
    except ControllerInstability as exc:
        stop_reason = f"controller_instability: {exc}"
    finally:
        if own_deps:
            sensor.close()
            rpc.close()

    manifest = _build_manifest(
        target=target,
        target_sha=target_sha,
        composition_hash=composition_hash,
        reference_f_version=ref_f.version,
        env=env,
        sessions=_load_prior_sessions(manifest_path)
        + [
            Session(
                session_id=session_id,
                started_at=session_started,
                stopped_at=_now_iso(),
                last_batch_id=batch_id - 1 if batch_id > 0 else None,
                stop_reason=stop_reason,
            )
        ],
        journal_path=journal_path,
        rpc=rpc if not own_deps else None,
    )
    manifest.write(manifest_path)
    return manifest_path


def _run_one_batch(
    *,
    controller: Controller,
    target: TargetConfig,
    sensor: SensorClient,
    rpc: RpcClient,
    facade_ctx: FacadeContext,
    journal_writer: JournalWriter,
    payload_writer: PayloadStreamWriter,
    session_id: int,
    resumed_from_batch: int | None,
    batch_id: int,
    pre_observation: StateObservation,
) -> tuple[str, StateObservation]:
    plan = controller.pick_next_batch(pre_observation, target)
    start_cursor = facade_ctx.address_cursor
    txs = dispatch(plan.verb, plan.deadline_bytes, facade_ctx)
    end_cursor = facade_ctx.address_cursor
    # Store cursors as 20-byte-padded hex so the journal schema (^0x[0-9a-fA-F]+$)
    # remains uniform and replay can recover them with a single int() call.
    start_addr = "0x" + start_cursor.to_bytes(20, "big").hex()
    end_addr = "0x" + end_cursor.to_bytes(20, "big").hex()

    block_hash = rpc.testing_commit_block_v1([tx.rlp for tx in txs])
    block = rpc.eth_get_block_by_hash(block_hash, full=True)
    payload = _block_to_payload(block, signed_txs=[tx.rlp for tx in txs])
    payload_writer.append(payload)

    try:
        post = sensor.read(expected_block=pre_observation.block_number + 1)
        status = "ok"
    except SensorWaitTimeout:
        post = pre_observation  # stale; will be refreshed next batch
        status = "sensor_wait_timeout"

    if status == "ok":
        obs_diag = controller.apply_observation(
            pre_observation, post, plan, tx_count=max(len(txs), 1)
        )
    else:
        obs_diag = {
            "observed_flat_bytes": 0,
            "coeffs_before": {plan.verb: dict(controller.F[plan.verb])},
            "coeffs_after": {plan.verb: dict(controller.F[plan.verb])},
            "sigma_innov": {plan.verb: dict(controller.state.sigma[plan.verb])},
            "alpha_current": controller.state.alpha,
            "innovation_ratio": 0.0,
            "residual_norm": 0.0,
        }

    record = Record(
        schema=1,
        session_id=session_id,
        resumed_from_batch=resumed_from_batch,
        ts_iso=_now_iso(),
        batch_id=batch_id,
        replay_core=ReplayCore(
            verb=plan.verb,
            deadline_bytes=plan.deadline_bytes,
            start_address=start_addr,
            end_address=end_addr,
            status=status,
            block_hash=block_hash,
            block_number=int(block["number"], 16) if isinstance(block.get("number"), str) else int(block.get("number", 0)),
        ),
        observability=Observability(
            observed_flat_bytes=int(obs_diag["observed_flat_bytes"]),
            coeffs_before=obs_diag["coeffs_before"],
            coeffs_after=obs_diag["coeffs_after"],
            sigma_innov=obs_diag["sigma_innov"],
            alpha_current=float(obs_diag["alpha_current"]),
            innovation_ratio=float(obs_diag["innovation_ratio"]),
            residual_norm=float(obs_diag["residual_norm"]),
            statecomp_snapshot=None if status == "sensor_wait_timeout" else post.raw,
        ),
    )
    journal_writer.append(record)
    return status, post


def _block_to_payload(block: dict[str, Any], *, signed_txs: list[bytes]) -> ExecutionPayloadV3:
    def _hex(field: str, default: bytes = b"") -> bytes:
        value = block.get(field)
        if value is None:
            return default
        if isinstance(value, bytes):
            return value
        return bytes.fromhex(value.removeprefix("0x"))

    def _int(field: str, default: int = 0) -> int:
        value = block.get(field)
        if value is None:
            return default
        if isinstance(value, int):
            return value
        return int(value, 16)

    return ExecutionPayloadV3(
        parent_hash=_hex("parentHash"),
        fee_recipient=_hex("miner"),
        state_root=_hex("stateRoot"),
        receipts_root=_hex("receiptsRoot"),
        logs_bloom=_hex("logsBloom"),
        prev_randao=_hex("mixHash"),
        block_number=_int("number"),
        gas_limit=_int("gasLimit"),
        gas_used=_int("gasUsed"),
        timestamp=_int("timestamp"),
        extra_data=_hex("extraData"),
        base_fee_per_gas=_int("baseFeePerGas"),
        block_hash=_hex("hash"),
        transactions=signed_txs,
        withdrawals=[],
        blob_gas_used=_int("blobGasUsed"),
        excess_blob_gas=_int("excessBlobGas"),
    )


def _build_manifest(
    *,
    target: TargetConfig,
    target_sha: str,
    composition_hash: str,
    reference_f_version: str,
    env: EnvInfo,
    sessions: list[Session],
    journal_path: Path,
    rpc: RpcClient | None,
) -> Manifest:
    final_state_root = None
    if rpc is not None:
        try:
            latest = rpc.eth_get_block_by_number("latest")
            final_state_root = latest.get("stateRoot")
        except Exception:
            final_state_root = None

    journal_sha = compute_journal_sha256(journal_path) if journal_path.exists() else ""

    return Manifest(
        run_id=str(uuid.uuid4()),
        target_yaml_sha256=target_sha,
        base_address="0x" + target.base_address.hex(),
        revision=target.revision,
        genesis_sha256=env.genesis_sha256,
        composition_hash=composition_hash,
        reference_f_version=reference_f_version,
        plugin_git_sha=env.plugin_git_sha,
        nethermind_commit_sha=env.nethermind_commit_sha,
        dotnet_runtime_major=env.dotnet_runtime_major,
        cpu_arch=env.cpu_arch,
        sessions=sessions,
        journal_sha256=journal_sha,
    )


def _load_prior_sessions(manifest_path: Path) -> list[Session]:
    if not manifest_path.exists():
        return []
    return Manifest.read(manifest_path).sessions


def _extract_last_session_id(state_dir: Path) -> int:
    manifest_path = state_dir / MANIFEST_FILENAME
    if not manifest_path.exists():
        return 0
    prior = Manifest.read(manifest_path)
    return max((s.session_id for s in prior.sessions), default=0)


def _now_iso() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


class _StopFlag:
    def __init__(self) -> None:
        self.requested = False

    def set(self) -> None:
        self.requested = True


def _install_signal_handlers() -> _StopFlag:
    flag = _StopFlag()

    def _handle(*_: Any) -> None:
        flag.set()

    try:
        signal.signal(signal.SIGINT, _handle)
        signal.signal(signal.SIGTERM, _handle)
    except ValueError:
        # Not in main thread (e.g. inside pytest) — callers handle stop differently.
        pass
    return flag
