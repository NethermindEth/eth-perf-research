"""Run lifecycle: auto-detect startup, main loop, manifest writer.

Entry point is `run(target, state_dir, rpc_url, ...)`. The lifecycle is signal-safe:
SIGINT/SIGTERM flip a `stop` flag that is checked between batches; the current batch
always finishes cleanly to avoid partial journal writes.
"""

from __future__ import annotations

import contextlib
import dataclasses
import enum
import errno
import fcntl
import logging
import os
import platform
import signal
import threading
import time
import uuid
from collections.abc import Generator
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import httpx

from .controller import (
    EPSILON,
    BatchPlan,
    Controller,
    ControllerInstability,
    init_state,
    rehydrate_state,
)
from .facade import FacadeContext, SignedTransaction, dispatch
from .journal import (
    PENDING_FILENAME,
    JournalReader,
    JournalSchemaError,
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
    TargetSnapshot,
    compute_chain_identity_hash,
    compute_journal_sha256,
    replay_context_matches,
)
from .payloads import ExecutionPayloadV3, PayloadStreamWriter
from .probe import ProbeExecutor, run_probe, seed_state_from_probe
from .reference_f import ReferenceF, default_reference_f_path, load_reference_f
from .rpc import RpcClient
from .sensor import SensorClient, SensorWaitTimeout, StateObservation
from .target import LiveTargetWatcher, TargetConfig

_log = logging.getLogger(__name__)


JOURNAL_FILENAME = "orchestrator.journal.jsonl"
PAYLOAD_FILENAME = "payloads.rlp"
MANIFEST_FILENAME = "run-manifest.json"
RUN_LOCK_FILENAME = "orchestrator.lock"

# Soft-stop on innovation residual: when ``controller.state.last_residual_norm``
# stays below ``THRESHOLD`` for ``WINDOW`` consecutive batches, the run halts
# with stop_reason "residual_converged". 0 (default) disables the stop, in
# which case only ``target_reached`` and ``max_batches`` end the loop.
RESIDUAL_STOP_THRESHOLD = float(os.environ.get("ORCH_RESIDUAL_STOP_THRESHOLD", "0.0"))
RESIDUAL_STOP_WINDOW = int(os.environ.get("ORCH_RESIDUAL_STOP_WINDOW", "6"))

# Soft-stop poll on target_reached: when the active target shape is fully
# satisfied we wait this long for an operator to edit ``target.yaml`` (a new
# target shape resumes the QP loop). After ``IDLE_TIMEOUT_SEC`` of no change
# the run shuts down with stop_reason "target_reached_idle". The poll cadence
# is tuned to avoid hot-stat'ing the file system.
TARGET_REACHED_IDLE_TIMEOUT_SEC = float(os.environ.get("ORCH_IDLE_TIMEOUT_SEC", "60"))
TARGET_REACHED_POLL_INTERVAL_SEC = float(os.environ.get("ORCH_TARGET_POLL_INTERVAL_SEC", "2"))


class StartupMode(enum.Enum):
    FRESH = "fresh"
    RESUME = "resume"


class ResumeRefused(Exception):
    """Pre-flight verification failed; operator must archive/rename the journal."""


class RunAlreadyActive(Exception):
    """Another orchestrator instance already holds the state-dir lock."""


@contextlib.contextmanager
def _state_dir_lock(state_dir: Path) -> Generator[None, None, None]:
    """Exclusive lock on ``state_dir/orchestrator.lock`` with NFS-safety probe.

    ``fcntl.flock`` is advisory-only on NFSv3 / SMB / some overlayfs setups and
    silently returns success without actually locking. After acquiring, we write
    a unique identity token, ``os.fsync``, re-read, and compare. If the read-back
    differs from what we wrote, two processes are colliding on a no-op lock and
    we refuse.

    File is opened with ``O_NOFOLLOW`` + mode ``0o600`` so a pre-planted symlink
    cannot redirect the lock write.
    """
    lock_path = state_dir / RUN_LOCK_FILENAME
    # O_NOFOLLOW: refuse to open if the path is a symlink. The lock file must be
    # a regular file controlled by the state dir's owner.
    flags = os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0)
    lock_fd = os.open(lock_path, flags, 0o600)
    try:
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError as exc:
            if exc.errno in (errno.EWOULDBLOCK, errno.EACCES):
                raise RunAlreadyActive(
                    f"another orchestrator holds {lock_path}; refuse to start"
                ) from exc
            raise
        token = f"{os.getpid()}:{platform.node()}:{uuid.uuid4().hex}\n".encode()
        os.ftruncate(lock_fd, 0)
        os.lseek(lock_fd, 0, os.SEEK_SET)
        os.write(lock_fd, token)
        os.fsync(lock_fd)
        os.lseek(lock_fd, 0, os.SEEK_SET)
        readback = os.read(lock_fd, len(token) + 128)
        if readback != token:
            # A concurrent holder just overwrote us — our "exclusive" flock is a
            # no-op (NFS without lockd, SMB without oplocks, etc.).
            raise RunAlreadyActive(
                f"lock identity read-back differs at {lock_path}: fcntl.flock may "
                f"be advisory-only on this filesystem. Move state_dir off networked "
                f"storage or pass --allow-networked-state-dir to opt in."
            )
        try:
            yield
        finally:
            with contextlib.suppress(OSError):
                fcntl.flock(lock_fd, fcntl.LOCK_UN)
    finally:
        os.close(lock_fd)


@dataclass
class StartupDecision:
    mode: StartupMode
    last_record: Record | None
    reason: str


def resolve_startup_mode(
    state_dir: Path,
    chain_identity_hash: str,
    head_block: int | None = None,
    *,
    rpc: RpcClient | None = None,
    facade_ctx: FacadeContext | None = None,
    replay_context: ReplayContext | None = None,
    resume_session_id: int | None = None,
) -> StartupDecision:
    """Pre-flight check for fresh vs resume. Raises `ResumeRefused` on mismatch.

    ``head_block`` is required when a journal exists: ``None`` means the RPC check
    failed, and we refuse rather than silently skip the alignment assertion.

    If a pending-batch sidecar is present (crash between commit and journal append),
    we reconcile with Nethermind head to synthesize the missing record and clear
    the sidecar. Requires ``rpc`` to be non-None on that path; ``facade_ctx`` is
    used to cross-check that the head block's tx set matches what the recorded
    verb+deadline would have produced.
    """

    journal = state_dir / JOURNAL_FILENAME
    pending = read_pending(state_dir)

    if not journal.exists() or journal.stat().st_size == 0:
        return _handle_empty_journal(state_dir, head_block, pending)

    prior = _load_and_verify_prior_manifest(state_dir, chain_identity_hash, replay_context)
    tail = _read_verified_tail(journal, prior)
    if tail is None:
        return StartupDecision(StartupMode.FRESH, None, "journal empty after verify")

    if head_block is None:
        raise ResumeRefused(
            "Nethermind head unknown (RPC unreachable or returned null) — "
            "refuse to resume without head alignment check"
        )

    if pending is not None:
        # Pending sidecar must belong to the SAME chain, not a stale one left by
        # a prior chain identity. Empty chain_identity_hash in the pending =
        # legacy file from before this check existed; reject it too rather than
        # silently accepting (operator should archive).
        if pending.chain_identity_hash != chain_identity_hash:
            raise ResumeRefused(
                f"pending sidecar chain_identity_hash "
                f"{pending.chain_identity_hash[:8] or '(empty)'}… does not match current "
                f"{chain_identity_hash[:8]}…; archive {state_dir / PENDING_FILENAME}"
            )
        tail = _reconcile_pending(
            state_dir=state_dir,
            tail=tail,
            pending=pending,
            head_block=head_block,
            rpc=rpc,
            facade_ctx=facade_ctx,
            resume_session_id=resume_session_id,
        )
    if head_block < tail.replay_core.block_number:
            raise ResumeRefused(
            f"Nethermind head {head_block} < journal tail {tail.replay_core.block_number}; "
            f"spec §1 reorg-free invariant violated (corruption, wrong data dir, "
            f"or external DB rollback). Archive state/ before restart."
        )
    if head_block not in (tail.replay_core.block_number, tail.replay_core.block_number + 1):
        raise ResumeRefused(
            f"Nethermind head {head_block} not in "
            f"{{{tail.replay_core.block_number}, {tail.replay_core.block_number + 1}}}"
        )

    return StartupDecision(StartupMode.RESUME, tail, "valid journal + head aligned")


def _handle_empty_journal(
    state_dir: Path, head_block: int | None, pending: PendingBatch | None
) -> StartupDecision:
    """Resolve the empty-or-missing-journal case.

    An empty journal while the chain already advanced means fresh-start would
    commit block N+1 against a stale parent. An empty journal with a pending
    sidecar is similarly ambiguous.
    """
    if head_block and head_block > 0:
        raise ResumeRefused(
            f"journal empty but Nethermind head is {head_block}; archive "
            f"state/ and restart fresh, or restore a backup"
        )
    if pending is not None:
        raise ResumeRefused(
            "journal empty but pending sidecar present — state ambiguous; "
            f"archive {state_dir / PENDING_FILENAME} before restart"
        )
    return StartupDecision(StartupMode.FRESH, None, "no journal at state dir")


def _load_and_verify_prior_manifest(
    state_dir: Path, chain_identity_hash: str, replay_context: ReplayContext | None
) -> Manifest | None:
    """Read the prior manifest (if any) and cross-check it against current config.

    ``chain_identity_hash`` covers genesis + signer + chain_id + runtime versions.
    A different ``target.yaml`` between runs is fine — target shape is a per-batch
    input now, recorded into ``manifest.target_history`` and not gated here.

    Tx-signing identity lives in ``replay_context``; check it separately so a
    changed signer / chain between runs refuses resume.

    Also guards against legacy-manifest bypass: if the journal is non-empty and
    the prior manifest predates ``ReplayContext`` (``prior.replay_context is
    None``), allowing the new run to resume would silently skip the signer +
    chain identity gate. Refuse so the operator must archive the old run.
    """
    manifest_path = state_dir / MANIFEST_FILENAME
    if not manifest_path.exists():
        return None
    prior = Manifest.read(manifest_path)
    if prior.chain_identity_hash != chain_identity_hash:
        raise ResumeRefused(
            f"chain_identity_hash mismatch: manifest={prior.chain_identity_hash[:8]}… "
            f"current={chain_identity_hash[:8]}…"
        )
    journal_path = state_dir / JOURNAL_FILENAME
    journal_nonempty = journal_path.exists() and journal_path.stat().st_size > 0
    if (
        journal_nonempty
        and prior.replay_context is None
        and replay_context is not None
    ):
        raise ResumeRefused(
            "legacy manifest has no replay_context; refusing to resume on a "
            "non-empty journal — start fresh or re-run on the original code"
        )
    if replay_context is not None and not replay_context_matches(
        prior.replay_context, replay_context
    ):
        raise ResumeRefused(
            "manifest.replay_context (base_address / revision / chain_id / "
            "gas_limit / signer) does not match the current FacadeContext; "
            "archive state/ and restart fresh"
        )
    return prior


def _read_verified_tail(journal: Path, prior: Manifest | None) -> Record | None:
    """Verify the journal chain hash and return the tail record (or None if empty).

    If a prior manifest checkpoint exists, verify only the suffix past it;
    otherwise verify the whole chain. Schema violation on resume is translated to
    ``ResumeRefused`` so the caller can instruct the operator to archive+restart.
    """
    reader = JournalReader(journal)
    try:
        if (
            prior is not None
            and prior.last_chain_hash_checkpoint
            and prior.last_checkpoint_batch_id >= 0
        ):
            reader.verify_chain_from(
                prev_hash=prior.last_chain_hash_checkpoint,
                min_batch_id=prior.last_checkpoint_batch_id + 1,
            )
        else:
            reader.verify_chain()
        return reader.tail()
    except JournalSchemaError as exc:
        raise ResumeRefused(f"journal schema violation on resume: {exc}") from exc


def _reconcile_pending(
    *,
    state_dir: Path,
    tail: Record,
    pending: PendingBatch,
    head_block: int,
    rpc: RpcClient | None,
    facade_ctx: FacadeContext | None = None,
    resume_session_id: int | None = None,
) -> Record:
    """Resolve an orphan pending-batch sidecar.

    Two legal shapes:
    - ``head == tail.block_number``: the commit never fired. Drop the sidecar.
    - ``head == tail.block_number + 1``: the commit happened but the journal append
      did not. Fetch the real block, cross-check its tx set against what the pending
      verb+deadline would dispatch, synthesize a record
      with ``status="reconciled_unobserved"``, append it, then drop the sidecar.
    Anything else: refuse — the state on disk is ambiguous.
    """
    # Pending == tail.batch_id means the journal append succeeded but the
    # clear_pending call failed (disk full, ENOSPC after fsync, etc). The
    # batch is already durable in the journal; just drop the stale sidecar.
    if pending.batch_id == tail.batch_id:
        clear_pending(state_dir)
        return tail
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
        block = rpc.eth_get_block_by_number(head_block, full=True)
        block_hash = block.get("hash")
        if not isinstance(block_hash, str):
            raise ResumeRefused(f"RPC returned no hash for head block {head_block}")
        # Carry the on-chain block.timestamp into the synthesized record so
        # replay re-supplies it. The chain is the source of truth here — the
        # sidecar pre-dates the commit and never saw the timestamp the EL
        # actually used (it could differ if the EL clamps to parent+1).
        raw_ts = block.get("timestamp", 0)
        block_ts = int(raw_ts, 16) if isinstance(raw_ts, str) else int(raw_ts)
        if facade_ctx is not None:
            _verify_reconciled_tx_set(facade_ctx, pending, block)
        journal_path = state_dir / JOURNAL_FILENAME
        with JournalWriter(journal_path) as writer:
            synthesized_session_id = (
                resume_session_id if resume_session_id is not None else pending.session_id
            )
            synthesized = Record(
                session_id=synthesized_session_id,
                resumed_from_batch=pending.resumed_from_batch,
                ts_iso=pending.ts_iso,
                batch_id=pending.batch_id,
                replay_core=ReplayCore(
                    verb=pending.verb,
                    deadline_bytes=pending.deadline_bytes,
                    start_address=pending.start_address,
                    end_address=pending.end_address,
                    status="reconciled_unobserved",
                    block_hash=block_hash,
                    block_number=head_block,
                    block_timestamp=block_ts,
                    target_sha256=pending.target_sha256,
                ),
                observability=Observability(
                    observed_flat_bytes=0,
                    coeffs_before=tail.observability.coeffs_after,
                    coeffs_after=tail.observability.coeffs_after,
                    alpha_state=tail.observability.alpha_state,
                    sigma_innov=tail.observability.sigma_innov,
                    alpha_current=tail.observability.alpha_current,
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


def _verify_reconciled_tx_set(
    ctx: FacadeContext, pending: PendingBatch, block: dict[str, Any]
) -> None:
    """Refuse reconcile if the head block's tx set differs from what we would dispatch.

    Replays the facade deterministically with ``pending.start_address`` as the
    starting cursor and compares *every tx hash* (not just count) against the
    head block's ``transactions[i].hash``. A count-only check lets a compromised
    or misbehaving Nethermind pass by committing an equal-sized but differently-
    signed tx set — the synthesized record would record the chain's block_hash
    while replay on another client would produce a different block_hash,
    violating replay-equivalence silently.
    """
    from eth_utils.crypto import keccak

    start_cursor = int(pending.start_address, 16)
    probe_ctx = dataclasses.replace(ctx, address_cursor=start_cursor, salt_cursor=0)
    txs = dispatch(pending.verb, pending.deadline_bytes, probe_ctx)
    chain_txs = block.get("transactions") or []
    if not isinstance(chain_txs, list):
        raise ResumeRefused("reconcile: RPC returned malformed `transactions` for head block")
    if len(chain_txs) != len(txs):
        raise ResumeRefused(
            f"reconcile: head block has {len(chain_txs)} txs but pending "
            f"verb={pending.verb!r} deadline_bytes={pending.deadline_bytes} "
            f"would produce {len(txs)}; refuse to trust the RPC — archive state/"
        )
    for i, (dispatched, chain_tx) in enumerate(zip(txs, chain_txs, strict=False)):
        expected = "0x" + keccak(dispatched.rlp).hex()
        chain_hash = chain_tx if isinstance(chain_tx, str) else chain_tx.get("hash")
        if not isinstance(chain_hash, str) or chain_hash.lower() != expected.lower():
            raise ResumeRefused(
                f"reconcile: tx {i} hash mismatch — dispatched {expected}, "
                f"chain reports {chain_hash!r}; either Nethermind is misbehaving "
                f"or state_dir has drifted. Archive state/ before restart."
            )



def make_journaling_probe_executor(
    *,
    controller: Controller,
    sensor: SensorClient,
    rpc: RpcClient,
    facade_ctx: FacadeContext,
    reference_f: ReferenceF,
    journal_writer: JournalWriter,
    payload_writer: PayloadStreamWriter,
    state_dir: Path,
    chain_identity_hash: str,
    active_target: TargetConfig,
    session_id: int,
    resumed_from_batch: int | None,
    batch_id_start: int = 0,
) -> ProbeExecutor:
    """Probe executor that journals each probe block.

    Every invocation goes through ``_commit_and_journal`` — the same pipeline
    as the QP main loop — so probe records share the pending sidecar, payload
    stream, and chain-hashed journal. ``batch_id`` advances 0..n-1 across calls
    via a shared counter; the QP loop must start at the post-probe value
    (``batch_id_start + n_probes``) for chain continuity.

    Sensor timeouts are propagated as ``sensor_wait_timeout`` records (same as
    QP), so probe-time stalls produce a journal record instead of being papered
    over.
    """
    counter = {"batch_id": batch_id_start}

    def _execute(verb: str, tx_count: int) -> tuple[StateObservation, StateObservation]:
        ref = reference_f.per_scenario(verb)
        deadline_bytes = max(
            1,
            int(abs(ref["accounts"]) + abs(ref["storage"]) + abs(ref["code"])) * tx_count,
        )
        pre = sensor.read()
        plan = BatchPlan(verb=verb, deadline_bytes=deadline_bytes, mix={verb: 1.0})
        status, post = _commit_and_journal(
            plan=plan,
            controller=controller,
            sensor=sensor,
            rpc=rpc,
            facade_ctx=facade_ctx,
            journal_writer=journal_writer,
            payload_writer=payload_writer,
            state_dir=state_dir,
            chain_identity_hash=chain_identity_hash,
            active_target=active_target,
            session_id=session_id,
            resumed_from_batch=resumed_from_batch,
            batch_id=counter["batch_id"],
            pre_observation=pre,
        )
        counter["batch_id"] += 1
        if status != "ok":
            # Sensor never advanced; return the stale pre as post so the
            # sanity gate sees a zero-delta and (for non-zero ref values)
            # raises CharacterizationInsufficient, aborting the probe.
            return pre, pre
        return pre, post

    return _execute


def _verify_nonce_aligned(rpc: RpcClient, ctx: FacadeContext) -> None:
    """Refuse resume if rehydrated ``address_cursor`` ≠ EOA on-chain nonce.

    Every verb signs with the single lab key and uses ``address_cursor`` as the
    tx nonce (facade/_builder.py). A crash mid-batch where partial txs landed
    on-chain leaves the cursor below the chain's nonce; replay would then
    re-sign already-mined nonces, the dispatch RPC would reject them, and the
    journal/chain would silently diverge. Catching this at resume keeps the
    invariant ``cursor == on_chain_nonce`` load-bearing.

    The genesis pre-funds the lab account with no explicit ``nonce`` field,
    so ``account.nonce == 0`` initially. Equality holds for fresh runs too,
    but we only invoke this on resume since fresh runs derive the cursor from
    init_state (= 0) and the nonce assertion is then trivial.
    """
    on_chain_nonce = rpc.eth_get_transaction_count(ctx.account.address, "latest")
    if ctx.address_cursor != on_chain_nonce:
        raise ResumeRefused(
            f"address_cursor / on-chain nonce mismatch for "
            f"{ctx.account.address}: cursor={ctx.address_cursor} "
            f"chain={on_chain_nonce}. Mid-batch crash likely leaked partial "
            f"txs onto the chain. Archive state/ before restart."
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


def _target_reached(observation: StateObservation, target: TargetConfig) -> bool:
    """True when each axis has met its target share — stop bloating instead of overshooting.

    Verbs only grow state, so once an axis crosses its target the controller
    can't bring it back down. Without this check the run keeps looking for
    something to chase and inflates other axes (e.g. accounts drifts past
    14 % while the controller closes the code residual). Stops as soon as
    every axis is at-or-past its share of ``target_total_bytes``.
    """
    targets = {
        "accounts": target.target_total_bytes * target.mainnet_target["accounts"],
        "storage": target.target_total_bytes * target.mainnet_target["storage"],
        "code": target.target_total_bytes * target.mainnet_target["code"],
    }
    return (
        observation.account_bytes >= targets["accounts"]
        and observation.storage_bytes >= targets["storage"]
        and observation.code_bytes >= targets["code"]
    )


_VERB_GAS_FACTOR_ALPHA = 0.3
_VERB_GAS_FACTOR_FLOOR = 0.05

_BLOCK_APPLY_LATENCY_WINDOW = 16
_BLOCK_APPLY_BACKPRESSURE_THRESHOLD_S = 1.5
_BACKPRESSURE_VERB = "noop"
_BACKPRESSURE_MIN_SAMPLES = 4


def record_block_apply_latency(ctx: FacadeContext, seconds: float) -> None:
    ctx.block_apply_latencies.append(seconds)
    if len(ctx.block_apply_latencies) > _BLOCK_APPLY_LATENCY_WINDOW:
        ctx.block_apply_latencies.pop(0)


def under_back_pressure(ctx: FacadeContext) -> bool:
    """True iff the recent p95 block-apply latency exceeds the threshold."""
    if len(ctx.block_apply_latencies) < _BACKPRESSURE_MIN_SAMPLES:
        return False
    sorted_buf = sorted(ctx.block_apply_latencies)
    p95_idx = max(0, int(len(sorted_buf) * 0.95) - 1)
    return sorted_buf[p95_idx] > _BLOCK_APPLY_BACKPRESSURE_THRESHOLD_S


def update_verb_gas_factor(
    ctx: FacadeContext,
    verb: str,
    block_gas_used: int,
    txs: list[SignedTransaction],
) -> float:
    """Update ``ctx.verb_gas_factors[verb]`` from a just-committed block.

    Factor = ``block.gas_used / sum(tx.gas_limit)`` smoothed by EWMA.
    Returns the new factor.
    """
    if not txs:
        return ctx.verb_gas_factors.get(verb, 1.0)
    sum_limits = sum(tx.gas_limit for tx in txs)
    if sum_limits <= 0:
        return ctx.verb_gas_factors.get(verb, 1.0)
    observed = max(_VERB_GAS_FACTOR_FLOOR, min(1.0, block_gas_used / sum_limits))
    prev = ctx.verb_gas_factors.get(verb, 1.0)
    new = _VERB_GAS_FACTOR_ALPHA * observed + (1 - _VERB_GAS_FACTOR_ALPHA) * prev
    ctx.verb_gas_factors[verb] = new
    return new


def build_facade_context(target: TargetConfig) -> FacadeContext:
    return FacadeContext(
        base_address=target.base_address,
        revision=target.revision,
        chain_id=target.chain_id,
        gas_limit=target.gas_limit,
        block_gas_limit=target.block_gas_limit,
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
        block_gas_limit=ctx.block_gas_limit,
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
    target_watcher: LiveTargetWatcher | None = None,
) -> Path:
    """Main entry. Returns the path to the manifest written on shutdown.

    ``target`` is the boot target shape. If ``target_watcher`` is supplied, it
    must already wrap that same target — the QP loop will call ``current()``
    once per batch to pick up live edits. When ``target_watcher`` is None we
    treat ``target`` as immutable for the run (legacy behaviour preserved for
    callers that don't have a backing file, e.g. tests with synthetic targets).
    """
    state_dir = Path(state_dir)
    state_dir.mkdir(parents=True, exist_ok=True)

    ref_f = reference_f or load_reference_f(default_reference_f_path())
    preflight_ctx = build_facade_context(target)
    replay_context = build_replay_context(preflight_ctx)
    chain_identity_hash = compute_chain_identity_hash(env, replay_context)

    own_deps = deps is None
    sensor_url = sensor_rpc_url or rpc_url
    sensor = deps.sensor if deps else SensorClient(sensor_url)
    rpc = deps.rpc if deps else RpcClient(rpc_url, jwt_path=jwt_path)
    probe_exec = deps.probe_executor if deps else None

    with _state_dir_lock(state_dir):
        return _run_locked(
            target=target,
            target_watcher=target_watcher,
            state_dir=state_dir,
            preflight_ctx=preflight_ctx,
            replay_context=replay_context,
            chain_identity_hash=chain_identity_hash,
            ref_f=ref_f,
            env=env,
            max_batches=max_batches,
            sensor=sensor,
            rpc=rpc,
            probe_exec=probe_exec,
            own_deps=own_deps,
        )


def _run_locked(
    *,
    target: TargetConfig,
    target_watcher: LiveTargetWatcher | None,
    state_dir: Path,
    preflight_ctx: FacadeContext,
    replay_context: ReplayContext,
    chain_identity_hash: str,
    ref_f: ReferenceF,
    env: EnvInfo,
    max_batches: int | None,
    sensor: SensorClient,
    rpc: RpcClient,
    probe_exec: ProbeExecutor | None,
    own_deps: bool,
) -> Path:
    head_block = _fetch_head_block(rpc)
    resume_session_id = _extract_last_session_id(state_dir) + 1
    decision = resolve_startup_mode(
        state_dir,
        chain_identity_hash=chain_identity_hash,
        head_block=head_block,
        rpc=rpc,
        facade_ctx=preflight_ctx,
        replay_context=replay_context,
        resume_session_id=resume_session_id,
    )

    ctx = preflight_ctx
    is_fresh = not (decision.mode == StartupMode.RESUME and decision.last_record is not None)
    if not is_fresh:
        assert decision.last_record is not None  # narrow for type checker
        session_id = resume_session_id
        resumed_from_batch: int | None = decision.last_record.batch_id
        state = rehydrate_state(
            ref_f,
            target.qp_scenarios,
            decision.last_record.observability,
            decision.last_record.batch_id,
        )
        batch_id = decision.last_record.batch_id + 1
        ctx.address_cursor = int(decision.last_record.replay_core.end_address, 16)
        ctx.last_block_timestamp = decision.last_record.replay_core.block_timestamp
        _verify_nonce_aligned(rpc, ctx)
    else:
        session_id = 1
        resumed_from_batch = None
        state = init_state(ref_f, target.qp_scenarios)
        batch_id = 0  # probe path advances this below

    controller = Controller(state)

    manifest_path = state_dir / MANIFEST_FILENAME
    target_history = _seed_target_history(manifest_path, target)

    journal_path = state_dir / JOURNAL_FILENAME
    payload_path = state_dir / PAYLOAD_FILENAME

    session_started = _now_iso()
    stop_reason = "max_batches"

    try:
        with (
            _signal_handlers() as stop,
            JournalWriter(journal_path) as jw,
            PayloadStreamWriter(payload_path) as pw,
        ):
            if is_fresh:
                active_probe = probe_exec or make_journaling_probe_executor(
                    controller=controller,
                    sensor=sensor,
                    rpc=rpc,
                    facade_ctx=ctx,
                    reference_f=ref_f,
                    journal_writer=jw,
                    payload_writer=pw,
                    state_dir=state_dir,
                    chain_identity_hash=chain_identity_hash,
                    active_target=target,
                    session_id=session_id,
                    resumed_from_batch=resumed_from_batch,
                    batch_id_start=batch_id,
                )
                results = run_probe(ref_f, target.qp_scenarios, active_probe)
                seed_state_from_probe(state, results)
                batch_id = len(results)

            last_observation = sensor.read()
            completed = 0
            consecutive_low_residual = 0
            active_target = target
            while not stop.is_set():
                if max_batches is not None and completed >= max_batches:
                    break
                # Per-batch live reload: stat target.yaml, swap in any new
                # shape, and journal the change into target_history. The
                # watcher is the single source of truth from this point on.
                next_target = (
                    target_watcher.current() if target_watcher is not None else target
                )
                if next_target.sha256 != active_target.sha256:
                    _log_target_change(active_target, next_target)
                    _append_snapshot_if_new(target_history, next_target)
                    # Persist the new history immediately so a crash before the
                    # next manifest write doesn't lose the snapshot reference.
                    _write_manifest_with_history(
                        manifest_path=manifest_path,
                        target=next_target,
                        chain_identity_hash=chain_identity_hash,
                        reference_f_version=ref_f.version,
                        env=env,
                        replay_context=replay_context,
                        target_history=target_history,
                        sessions=[
                            *_load_prior_sessions(manifest_path),
                            Session(
                                session_id=session_id,
                                started_at=session_started,
                                last_batch_id=batch_id - 1 if batch_id > 0 else None,
                                stop_reason="in_progress",
                            ),
                        ],
                        journal_path=journal_path,
                        rpc=None,
                    )
                    active_target = next_target
                batch_status, last_observation = _run_one_batch(
                    controller=controller,
                    target=active_target,
                    sensor=sensor,
                    rpc=rpc,
                    facade_ctx=ctx,
                    journal_writer=jw,
                    payload_writer=pw,
                    state_dir=state_dir,
                    chain_identity_hash=chain_identity_hash,
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
                if _target_reached(last_observation, active_target):
                    polled = _poll_for_target_change(
                        watcher=target_watcher,
                        active_sha=active_target.sha256,
                        stop=stop,
                    )
                    if polled is None:
                        stop_reason = "target_reached_idle"
                        break
                    continue
                if RESIDUAL_STOP_THRESHOLD > 0:
                    if controller.state.last_residual_norm < RESIDUAL_STOP_THRESHOLD:
                        consecutive_low_residual += 1
                    else:
                        consecutive_low_residual = 0
                    if consecutive_low_residual >= RESIDUAL_STOP_WINDOW:
                        stop_reason = "residual_converged"
                        break
            else:
                stop_reason = "signal"
    except ControllerInstability as exc:
        stop_reason = f"controller_instability: {exc}"
    finally:
        if own_deps:
            sensor.close()
            rpc.close()

    final_target = locals().get("active_target", target)
    manifest = _build_manifest(
        target=final_target,
        chain_identity_hash=chain_identity_hash,
        reference_f_version=ref_f.version,
        env=env,
        replay_context=replay_context,
        target_history=target_history,
        sessions=[
            *_load_prior_sessions(manifest_path),
            Session(
                session_id=session_id,
                started_at=session_started,
                stopped_at=_now_iso(),
                last_batch_id=batch_id - 1 if batch_id > 0 else None,
                stop_reason=stop_reason,
            ),
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
    chain_identity_hash: str,
    session_id: int,
    resumed_from_batch: int | None,
    batch_id: int,
    pre_observation: StateObservation,
) -> tuple[str, StateObservation]:
    plan = controller.pick_next_batch(
        pre_observation, target,
        batch_id=batch_id, chain_identity_hash=chain_identity_hash,
    )
    # Back-pressure: when block-apply latency trends up, NM's pruner is
    # behind — insert a noop batch so the pruner has a cheap block to catch up.
    if (
        _BACKPRESSURE_VERB in target.qp_scenarios
        and plan.verb != _BACKPRESSURE_VERB
        and under_back_pressure(facade_ctx)
    ):
        plan = dataclasses.replace(
            plan, verb=_BACKPRESSURE_VERB, mix={_BACKPRESSURE_VERB: 1.0}
        )
    return _commit_and_journal(
        plan=plan,
        controller=controller,
        sensor=sensor,
        rpc=rpc,
        facade_ctx=facade_ctx,
        journal_writer=journal_writer,
        payload_writer=payload_writer,
        state_dir=state_dir,
        chain_identity_hash=chain_identity_hash,
        active_target=target,
        session_id=session_id,
        resumed_from_batch=resumed_from_batch,
        batch_id=batch_id,
        pre_observation=pre_observation,
    )


def _commit_and_journal(
    *,
    plan: BatchPlan,
    controller: Controller,
    sensor: SensorClient,
    rpc: RpcClient,
    facade_ctx: FacadeContext,
    journal_writer: JournalWriter,
    payload_writer: PayloadStreamWriter,
    state_dir: Path,
    chain_identity_hash: str,
    active_target: TargetConfig,
    session_id: int,
    resumed_from_batch: int | None,
    batch_id: int,
    pre_observation: StateObservation,
) -> tuple[str, StateObservation]:
    """Dispatch ``plan``, commit its block, journal the result, and return ``(status, post)``.

    Shared seam between the QP main loop and the probe sequence: both supply
    a ``BatchPlan``. The full pending → commit → payload → sense →
    apply_observation → journal → clear-pending pipeline is identical, so probe
    records are crash-recoverable on the same code path the main loop uses.
    """
    start_cursor = facade_ctx.address_cursor
    txs = dispatch(plan.verb, plan.deadline_bytes, facade_ctx)
    end_cursor = facade_ctx.address_cursor
    start_addr = "0x" + start_cursor.to_bytes(20, "big").hex()
    end_addr = "0x" + end_cursor.to_bytes(20, "big").hex()

    # Write a pending-batch sidecar BEFORE the commit. If we crash between
    # commit and journal append, resume uses this to synthesize the missing record.
    ts_iso = _now_iso()
    # Generate the EL block timestamp once per batch and capture it in the
    # sidecar + journal so replay re-supplies the same value. The Engine API requires
    # strictly increasing timestamps; on rapid commits (probe phase fires 7
    # blocks within one second) `_now_unix()` returns the same int, so we floor
    # at ``parent.timestamp + 1`` — Nethermind tolerated the duplicate, geth /
    # besu / reth reject it with `invalid timestamp`.
    block_timestamp = max(facade_ctx.last_block_timestamp + 1, _now_unix())
    facade_ctx.last_block_timestamp = block_timestamp
    write_pending(
        state_dir,
        PendingBatch(
            session_id=session_id,
            resumed_from_batch=resumed_from_batch,
            batch_id=batch_id,
            verb=plan.verb,
            deadline_bytes=plan.deadline_bytes,
            chain_identity_hash=chain_identity_hash,
            target_sha256=active_target.sha256,
            start_address=start_addr,
            end_address=end_addr,
            ts_iso=ts_iso,
            pre_block_number=pre_observation.block_number,
        ),
    )

    commit_started = time.monotonic()
    block_hash = rpc.testing_commit_block_v1(
        [tx.rlp for tx in txs], timestamp_unix=block_timestamp
    )
    block = rpc.eth_get_block_by_hash(block_hash, full=True)
    record_block_apply_latency(facade_ctx, time.monotonic() - commit_started)
    payload = _block_to_payload(block, signed_txs=[tx.rlp for tx in txs])
    payload_writer.append(payload)

    block_gas_used = int(payload.gas_used)
    update_verb_gas_factor(facade_ctx, plan.verb, block_gas_used, txs)

    try:
        post = sensor.read(expected_block=pre_observation.block_number + 1)
        status = "ok"
    except SensorWaitTimeout:
        post = pre_observation  # stale; will be refreshed next batch
        status = "sensor_wait_timeout"

    if status == "ok":
        obs_diag: dict[str, Any] = controller.apply_observation(
            pre_observation, post, plan,
            tx_count=max(len(txs), 1),
            dispatched_rlp_bytes=sum(len(tx.rlp) for tx in txs),
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
            block_number=int(block["number"], 16)
            if isinstance(block.get("number"), str)
            else int(block.get("number", 0)),
            block_timestamp=block_timestamp,
            target_sha256=active_target.sha256,
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
            statecomp_snapshot=post.raw if post.raw else None,
            statecomp_snapshot_stale=(status == "sensor_wait_timeout"),
            mix_simplex=dict(plan.mix),
            epsilon=EPSILON,
            gas_used=block_gas_used,
            verb_gas_factors=dict(facade_ctx.verb_gas_factors),
        ),
    )
    journal_writer.append(record)
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
    chain_identity_hash: str,
    reference_f_version: str,
    env: EnvInfo,
    replay_context: ReplayContext,
    target_history: list[TargetSnapshot],
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
    checkpoint_hash = ""
    checkpoint_batch = -1
    if journal_path.exists() and journal_path.stat().st_size > 0:
        tail = JournalReader(journal_path).tail()
        if tail is not None:
            checkpoint_hash = tail.replay_core.chain_hash
            checkpoint_batch = tail.batch_id

    return Manifest(
        run_id=str(uuid.uuid4()),
        base_address="0x" + target.base_address.hex(),
        revision=target.revision,
        genesis_sha256=env.genesis_sha256,
        chain_identity_hash=chain_identity_hash,
        reference_f_version=reference_f_version,
        plugin_git_sha=env.plugin_git_sha,
        nethermind_commit_sha=env.nethermind_commit_sha,
        dotnet_runtime_major=env.dotnet_runtime_major,
        cpu_arch=env.cpu_arch,
        replay_context=replay_context,
        sessions=sessions,
        target_history=list(target_history),
        journal_sha256=journal_sha,
        last_chain_hash_checkpoint=checkpoint_hash,
        last_checkpoint_batch_id=checkpoint_batch,
        final_state_root=final_state_root,
    )


def _load_prior_sessions(manifest_path: Path) -> list[Session]:
    if not manifest_path.exists():
        return []
    return Manifest.read(manifest_path).sessions


def _extract_last_session_id(state_dir: Path) -> int:
    """Return ``max(manifest_sessions, journal_tail.session_id)``.

    If the last session's manifest write failed (disk full, crash after fsync),
    the manifest is one session behind the journal. Falling back to the manifest
    alone would recycle a session_id that already appears in the journal, which
    breaks per-session filtering in downstream analytics.
    """
    manifest_path = state_dir / MANIFEST_FILENAME
    journal_path = state_dir / JOURNAL_FILENAME
    manifest_sid = 0
    if manifest_path.exists():
        prior = Manifest.read(manifest_path)
        manifest_sid = max((s.session_id for s in prior.sessions), default=0)
    journal_sid = 0
    if journal_path.exists() and journal_path.stat().st_size > 0:
        try:
            tail = JournalReader(journal_path).tail()
        except JournalSchemaError:
            tail = None
        if tail is not None:
            journal_sid = tail.session_id
    return max(manifest_sid, journal_sid)


def _seed_target_history(
    manifest_path: Path, current: TargetConfig
) -> list[TargetSnapshot]:
    """Load every prior target_history snapshot, then ensure ``current`` is in it.

    Replay reads each batch's target by ``target_sha256``; any sha referenced
    by the journal must exist in ``manifest.target_history`` or replay fails
    loudly. Resume preserves the prior list verbatim so old batches still
    look up correctly, then appends the boot target if its sha is new.
    """
    history: list[TargetSnapshot] = []
    if manifest_path.exists():
        try:
            prior = Manifest.read(manifest_path)
        except (ValueError, OSError):
            prior = None
        if prior is not None:
            history = list(prior.target_history)
    _append_snapshot_if_new(history, current)
    return history


def _append_snapshot_if_new(
    history: list[TargetSnapshot], target: TargetConfig
) -> None:
    """Append ``target`` to ``history`` iff its sha256 is not already present."""
    if any(snap.sha256 == target.sha256 for snap in history):
        return
    history.append(
        TargetSnapshot(
            sha256=target.sha256,
            ts_iso=_now_iso(),
            body=dict(target.raw),
        )
    )


def _log_target_change(prev: TargetConfig, nxt: TargetConfig) -> None:
    """Emit the single-line target-change banner the spec calls for."""
    accounts = nxt.mainnet_target.get("accounts", 0.0)
    storage = nxt.mainnet_target.get("storage", 0.0)
    code = nxt.mainnet_target.get("code", 0.0)
    total_mib = nxt.target_total_bytes / (1024 * 1024)
    _log.info(
        "target changed: %s… → %s…; new shape acc=%.3f sto=%.3f cod=%.3f total=%.0f MiB",
        prev.sha256[:8],
        nxt.sha256[:8],
        accounts,
        storage,
        code,
        total_mib,
    )


def _write_manifest_with_history(
    *,
    manifest_path: Path,
    target: TargetConfig,
    chain_identity_hash: str,
    reference_f_version: str,
    env: EnvInfo,
    replay_context: ReplayContext,
    target_history: list[TargetSnapshot],
    sessions: list[Session],
    journal_path: Path,
    rpc: RpcClient | None,
) -> None:
    """Atomically persist the manifest mid-run after a target swap.

    Without this checkpoint a target change followed by a crash would lose the
    new snapshot — replay would see ``target_sha256`` references with no
    matching entry in ``target_history`` and refuse. The atomic write swaps
    the file in one operation so partial writes are impossible.
    """
    manifest = _build_manifest(
        target=target,
        chain_identity_hash=chain_identity_hash,
        reference_f_version=reference_f_version,
        env=env,
        replay_context=replay_context,
        target_history=target_history,
        sessions=sessions,
        journal_path=journal_path,
        rpc=rpc,
    )
    manifest.write(manifest_path)


def _poll_for_target_change(
    *,
    watcher: LiveTargetWatcher | None,
    active_sha: str,
    stop: threading.Event,
) -> TargetConfig | None:
    """Block up to ORCH_IDLE_TIMEOUT_SEC waiting for a target.yaml change.

    Returns the new ``TargetConfig`` as soon as a sha change is observed; or
    ``None`` if the idle timeout elapses. Wakes early on SIGINT/SIGTERM (the
    ``stop`` flag) so Ctrl-C still terminates the run promptly.

    With no watcher (synthetic in-memory targets, tests), returns None
    immediately — there's nothing to observe.
    """
    if watcher is None:
        return None
    deadline = _monotonic() + TARGET_REACHED_IDLE_TIMEOUT_SEC
    while not stop.is_set():
        remaining = deadline - _monotonic()
        if remaining <= 0:
            return None
        sleep_for = min(remaining, TARGET_REACHED_POLL_INTERVAL_SEC)
        # Use Event.wait for early cancellation: returns True on stop.set(),
        # False on timeout. Either way we re-check the watcher below.
        if stop.wait(sleep_for):
            return None
        nxt = watcher.current()
        if nxt.sha256 != active_sha:
            return nxt
    return None


def _monotonic() -> float:
    """Wall-clock-independent seconds since process start.

    Wrapped so tests can monkey-patch the timer when exercising the
    soft-stop poll without sleeping in real time.
    """
    import time as _time

    return _time.monotonic()


def _now_iso() -> str:
    return datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def _now_unix() -> int:
    """Wall-clock seconds since the unix epoch.

    Captured ONCE per batch and journaled so replay can re-supply the exact
    value to ``testing_commit_block_v1``. The EL folds this into the block
    header hash, so determinism here is load-bearing for §C.1 replay.
    """
    import time as _time

    return int(_time.time())


@contextlib.contextmanager
def _signal_handlers() -> Generator[threading.Event, None, None]:
    """Install SIGINT/SIGTERM handlers that set a ``threading.Event``; restore on exit."""
    flag = threading.Event()

    def _handle(*_: Any) -> None:
        flag.set()

    prior: dict[signal.Signals, Any] = {}
    signals = (signal.SIGINT, signal.SIGTERM)
    try:
        for sig in signals:
            try:
                prior[sig] = signal.signal(sig, _handle)
            except ValueError:
                # Not in the main thread (pytest worker, etc.). Skip, but remember
                # we did not install so the restore loop is a no-op for this sig.
                prior.pop(sig, None)
        yield flag
    finally:
        for sig, handler in prior.items():
            with contextlib.suppress(ValueError):
                signal.signal(sig, handler)
