"""Run lifecycle: auto-detect startup, main loop, manifest writer.

Entry point is `run(target, state_dir, rpc_url, ...)`. The lifecycle is signal-safe:
SIGINT/SIGTERM flip a `stop` flag that is checked between batches; the current batch
always finishes cleanly to avoid partial journal writes.
"""
from __future__ import annotations

import enum
import logging
import signal
import time
import uuid
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable

import httpx


_log = logging.getLogger(__name__)

from .controller import (
    BatchPlan,
    Controller,
    ControllerInstability,
    ControllerState,
    init_state,
    rehydrate_state,
)
from .facade import FacadeContext, dispatch
from .journal import (
    JournalReader,
    JournalWriter,
    Observability,
    PendingBatch,
    Record,
    ReplayCore,
    clear_pending,
    read_pending,
    write_pending,
)
from .manifest import (
    EnvInfo,
    Manifest,
    ReplayContext,
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
    *,
    rpc: RpcClient | None = None,
) -> StartupDecision:
    """Pre-flight check for fresh vs resume. Raises `ResumeRefused` on mismatch.

    ``head_block`` is required when a journal exists: ``None`` means the RPC check
    failed, and we refuse rather than silently skip the alignment assertion.

    If a pending-batch sidecar is present (crash between commit and journal append),
    we reconcile with Nethermind head to synthesize the missing record and clear
    the sidecar. Requires ``rpc`` to be non-None on that path.
    """
    journal = state_dir / JOURNAL_FILENAME
    pending = read_pending(state_dir)

    if not journal.exists() or journal.stat().st_size == 0:
        if pending is not None:
            # Fresh start with leftover pending = aborted before first commit. Safe to drop.
            clear_pending(state_dir)
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

    if head_block is None:
        raise ResumeRefused(
            "Nethermind head unknown (RPC unreachable or returned null) — "
            "refuse to resume without head alignment check"
        )

    if pending is not None:
        tail = _reconcile_pending(
            state_dir=state_dir,
            tail=tail,
            pending=pending,
            head_block=head_block,
            rpc=rpc,
        )
    if head_block not in (tail.replay_core.block_number, tail.replay_core.block_number + 1):
        raise ResumeRefused(
            f"Nethermind head {head_block} not in "
            f"{{{tail.replay_core.block_number}, {tail.replay_core.block_number + 1}}}"
        )

    return StartupDecision(StartupMode.RESUME, tail, "valid journal + head aligned")


def _reconcile_pending(
    *,
    state_dir: Path,
    tail: Record,
    pending: PendingBatch,
    head_block: int,
    rpc: RpcClient | None,
) -> Record:
    """Resolve an orphan pending-batch sidecar.

    Two legal shapes:
    - ``head == tail.block_number``: the commit never fired. Drop the sidecar.
    - ``head == tail.block_number + 1``: the commit happened but the journal append
      did not. Fetch the real block, synthesize a record with ``status="aborted"``
      (sensor state was lost), append it, then drop the sidecar.
    Anything else: refuse — the state on disk is ambiguous.
    """
    # Pending is always for the batch immediately after the journal tail.
    if pending.batch_id != tail.batch_id + 1:
        raise ResumeRefused(
            f"pending sidecar batch_id={pending.batch_id} inconsistent with "
            f"journal tail batch_id={tail.batch_id}; archive state/ and restart fresh"
        )

    if head_block == tail.replay_core.block_number:
        clear_pending(state_dir)
        return tail

    if head_block == tail.replay_core.block_number + 1:
        if rpc is None:
            raise ResumeRefused("reconcile needs RPC but none supplied")
        block = rpc.eth_get_block_by_number(head_block, full=False)
        block_hash = block.get("hash")
        if not isinstance(block_hash, str):
            raise ResumeRefused(f"RPC returned no hash for head block {head_block}")
        journal_path = state_dir / JOURNAL_FILENAME
        with JournalWriter(journal_path) as writer:
            synthesized = Record(
                session_id=pending.session_id,
                resumed_from_batch=pending.resumed_from_batch,
                ts_iso=pending.ts_iso,
                batch_id=pending.batch_id,
                replay_core=ReplayCore(
                    verb=pending.verb,
                    deadline_bytes=pending.deadline_bytes,
                    start_address=pending.start_address,
                    end_address=pending.end_address,
                    status="aborted",
                    block_hash=block_hash,
                    block_number=head_block,
                ),
                observability=Observability(
                    alpha_current=0.0,
                    innovation_ratio=0.0,
                    residual_norm=0.0,
                    statecomp_snapshot=None,
                ),
            )
            writer.append(synthesized)
        clear_pending(state_dir)
        return synthesized

    raise ResumeRefused(
        f"pending sidecar + head {head_block} not reconcilable against tail block "
        f"{tail.replay_core.block_number}"
    )


def _fetch_head_block(rpc: RpcClient) -> int | None:
    """Best-effort head fetch for the resume pre-flight check.

    Returns None only on transport errors — malformed responses propagate so the
    operator sees the real problem. `resolve_startup_mode` refuses resume on None.
    """
    try:
        block = rpc.eth_get_block_by_number("latest")
    except (httpx.HTTPError, ConnectionError, TimeoutError) as exc:
        _log.warning("head-block fetch failed: %s", exc)
        return None
    head = block.get("number")
    if head is None:
        _log.warning("head-block response missing `number` field")
        return None
    return int(head, 16) if isinstance(head, str) else int(head)


def build_facade_context(target: TargetConfig) -> FacadeContext:
    return FacadeContext(
        base_address=target.base_address,
        revision=target.revision,
        chain_id=int(target.raw.get("chain_id", 1337)),
        gas_limit=int(target.raw.get("gas_limit", 30_000_000)),
    )


def build_replay_context(ctx: FacadeContext) -> ReplayContext:
    """Snapshot the tx-signing identity so replay can reconstruct it."""
    return ReplayContext(
        base_address="0x" + ctx.base_address.hex(),
        revision=ctx.revision,
        chain_id=ctx.chain_id,
        gas_limit=ctx.gas_limit,
        address_stride=ctx.address_stride,
        deploy_pubkey_sha256=ctx.deploy_pubkey_sha256(),
    )


@dataclass
class LifecycleDeps:
    """Injectable dependencies so tests can stub RPC/sensor without patching.

    Production callers typically leave these unset and let ``run()`` construct
    default clients from ``rpc_url`` / ``jwt_path``; tests pass stubs directly.
    """

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
    jwt_path: Path | str | None = None,
    sensor_rpc_url: str | None = None,
) -> Path:
    """Main entry. Returns the path to the manifest written on shutdown."""
    state_dir = Path(state_dir)
    state_dir.mkdir(parents=True, exist_ok=True)

    target_sha = target.source_sha256
    ref_f = reference_f or load_reference_f(default_reference_f_path())
    # Pre-build the facade context + replay snapshot so composition_hash covers
    # every knob that can change the final state root (spec §C.1).
    preflight_ctx = build_facade_context(target)
    replay_context = build_replay_context(preflight_ctx)
    composition_hash = compute_composition_hash(target_sha, env, replay_context)

    own_deps = deps is None
    sensor_url = sensor_rpc_url or rpc_url
    sensor = deps.sensor if deps else SensorClient(sensor_url)
    rpc = deps.rpc if deps else RpcClient(rpc_url, jwt_path=jwt_path)
    probe_exec = deps.probe_executor if deps else None

    head_block = _fetch_head_block(rpc)
    decision = resolve_startup_mode(
        state_dir, composition_hash, head_block, rpc=rpc
    )

    ctx = preflight_ctx
    if decision.mode == StartupMode.RESUME and decision.last_record is not None:
        session_id = _extract_last_session_id(state_dir) + 1
        resumed_from_batch = decision.last_record.batch_id
        # Spec §C.3 step 6: reconstruct F/σ/α from the journal tail so controller
        # continuity survives resume. Without this every session is a cold start.
        state = rehydrate_state(
            ref_f,
            target.qp_scenarios,
            decision.last_record.observability,
            decision.last_record.batch_id,
        )
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
                    state_dir=state_dir,
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
        replay_context=replay_context,
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
    state_dir: Path,
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

    # C3: write a pending-batch sidecar BEFORE the commit. If we crash between
    # commit and journal append, resume uses this to synthesize the missing record.
    ts_iso = _now_iso()
    write_pending(
        state_dir,
        PendingBatch(
            session_id=session_id,
            resumed_from_batch=resumed_from_batch,
            batch_id=batch_id,
            verb=plan.verb,
            deadline_bytes=plan.deadline_bytes,
            start_address=start_addr,
            end_address=end_addr,
            ts_iso=ts_iso,
            pre_block_number=pre_observation.block_number,
        ),
    )

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
            "alpha_current": controller.state.alpha_mean(),
            "alpha_state": {plan.verb: dict(controller.state.alpha[plan.verb])},
            "innovation_ratio": 0.0,
            "residual_norm": 0.0,
        }

    record = Record(
        schema=1,
        session_id=session_id,
        resumed_from_batch=resumed_from_batch,
        ts_iso=ts_iso,
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
            alpha_state=obs_diag.get("alpha_state", {}),
            sigma_innov=obs_diag["sigma_innov"],
            alpha_current=float(obs_diag["alpha_current"]),
            innovation_ratio=float(obs_diag["innovation_ratio"]),
            residual_norm=float(obs_diag["residual_norm"]),
            statecomp_snapshot=None if status == "sensor_wait_timeout" else post.raw,
        ),
    )
    journal_writer.append(record)
    # Pending sidecar is only meaningful while journal append is incomplete.
    clear_pending(state_dir)
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
    replay_context: ReplayContext,
    sessions: list[Session],
    journal_path: Path,
    rpc: RpcClient | None,
) -> Manifest:
    final_state_root = None
    if rpc is not None:
        try:
            latest = rpc.eth_get_block_by_number("latest")
            final_state_root = latest.get("stateRoot")
        except (httpx.HTTPError, ConnectionError, TimeoutError) as exc:
            _log.warning("final state_root fetch failed: %s", exc)

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
        replay_context=replay_context,
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
