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
import uuid
from collections.abc import Generator
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import httpx

from .controller import (
    Controller,
    ControllerInstability,
    init_state,
    rehydrate_state,
)
from .facade import FacadeContext, dispatch
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
    compute_composition_hash,
    compute_journal_sha256,
    replay_context_matches,
)
from .payloads import ExecutionPayloadV3, PayloadStreamWriter
from .probe import ProbeExecutor, run_probe, seed_state_from_probe
from .reference_f import ReferenceF, default_reference_f_path, load_reference_f
from .rpc import RpcClient
from .sensor import SensorClient, SensorWaitTimeout, StateObservation
from .target import TargetConfig

_log = logging.getLogger(__name__)


JOURNAL_FILENAME = "orchestrator.journal.jsonl"
PAYLOAD_FILENAME = "payloads.rlp"
MANIFEST_FILENAME = "run-manifest.json"
RUN_LOCK_FILENAME = "orchestrator.lock"


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
    we refuse (review C3 / skeptic F-2).

    File is opened with ``O_NOFOLLOW`` + mode ``0o600`` so a pre-planted symlink
    cannot redirect the lock write (security M-SYMLINK).
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
    composition_hash: str,
    head_block: int | None,
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
    verb+deadline would have produced (review C-RECONCILE-TRUST).
    """
    journal = state_dir / JOURNAL_FILENAME
    pending = read_pending(state_dir)

    if not journal.exists() or journal.stat().st_size == 0:
        return _handle_empty_journal(state_dir, head_block, pending)

    prior = _load_and_verify_prior_manifest(state_dir, composition_hash, replay_context)
    tail = _read_verified_tail(journal, prior)
    if tail is None:
        return StartupDecision(StartupMode.FRESH, None, "journal empty after verify")

    if head_block is None:
        raise ResumeRefused(
            "Nethermind head unknown (RPC unreachable or returned null) — "
            "refuse to resume without head alignment check"
        )

    if pending is not None:
        # Pending sidecar must belong to the SAME run, not a stale one left by a
        # prior composition. Empty composition_hash in the pending = legacy file
        # from before this check existed; reject it too rather than silently
        # accepting (operator should archive).
        if pending.composition_hash != composition_hash:
            raise ResumeRefused(
                f"pending sidecar composition_hash "
                f"{pending.composition_hash[:8] or '(empty)'}… does not match current "
                f"{composition_hash[:8]}…; archive {state_dir / PENDING_FILENAME}"
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
        # Spec §1 assumes reorg-free by construction; head cannot regress. If it
        # did, we're pointed at the wrong data dir, the DB rolled back, or the
        # reorg-free invariant was violated externally. Surface that instead of
        # the generic "not in {N, N+1}" message (design-compliance #5).
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

    An empty journal while the chain already advanced = operator intervention
    territory (review C-EMPTY-JOURNAL); fresh-start would commit block N+1 against
    a stale parent. An empty journal with a pending sidecar is similarly ambiguous.
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
    state_dir: Path, composition_hash: str, replay_context: ReplayContext | None
) -> Manifest | None:
    """Read the prior manifest (if any) and cross-check it against current config.

    ``composition_hash`` covers target+env per design §7. Tx-signing identity lives
    in ``replay_context``; check it separately so a changed signer / chain between
    runs refuses resume even when target.yaml is unchanged.
    """
    manifest_path = state_dir / MANIFEST_FILENAME
    if not manifest_path.exists():
        return None
    prior = Manifest.read(manifest_path)
    if prior.composition_hash != composition_hash:
        raise ResumeRefused(
            f"composition_hash mismatch: manifest={prior.composition_hash[:8]}… "
            f"current={composition_hash[:8]}…"
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

    H7: if a prior manifest checkpoint exists, verify only the suffix past it;
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
      verb+deadline would dispatch (review C-RECONCILE-TRUST), synthesize a record
      with ``status="reconciled_unobserved"``, append it, then drop the sidecar.
    Anything else: refuse — the state on disk is ambiguous.
    """
    # Pending == tail.batch_id means the journal append succeeded but the
    # clear_pending call failed (disk full, ENOSPC after fsync, etc). The
    # batch is already durable in the journal; just drop the stale sidecar
    # and continue (skeptic F-9).
    if pending.batch_id == tail.batch_id:
        clear_pending(state_dir)
        return tail
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
        block = rpc.eth_get_block_by_number(head_block, full=True)
        block_hash = block.get("hash")
        if not isinstance(block_hash, str):
            raise ResumeRefused(f"RPC returned no hash for head block {head_block}")
        # Cross-check the head block's tx count against what the pending verb+
        # deadline_bytes would have dispatched. Catches a misbehaving or compromised
        # Nethermind that auto-mined an empty block (or committed a different tx
        # set) between the commit call and our resume (review C-RECONCILE-TRUST).
        if facade_ctx is not None:
            _verify_reconciled_tx_set(facade_ctx, pending, block)
        journal_path = state_dir / JOURNAL_FILENAME
        with JournalWriter(journal_path) as writer:
            # Carry the tail's F/σ/α forward so the next resume doesn't cold-start
            # the controller. The unobserved batch didn't change those coefficients
            # (apply_observation never ran for it), so tail.observability is
            # authoritative. Status uses the new `reconciled_unobserved` value so
            # downstream tooling can filter reconciled records without conflating
            # them with explicit `aborted` shutdowns (spec §8 reserved for that).
            # Stamp with the *new* session's id so the record has a matching
            # Session entry in the manifest once shutdown runs. Falling back to
            # pending.session_id preserves legacy behavior when the caller
            # didn't compute a resume_session_id (should only happen in tests).
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
    violating §C.1 silently (round-3 C1 / skeptic F-5 / TRIZ H1).
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


def make_default_probe_executor(
    *,
    rpc: RpcClient,
    sensor: SensorClient,
    facade_ctx: FacadeContext,
    reference_f: ReferenceF,
) -> ProbeExecutor:
    """Build a ProbeExecutor wired to the live facade + rpc + sensor stack.

    Used by ``_run_locked`` when no explicit executor is injected and the mode
    is FRESH. Without this, the probe sequence (spec §B.2) was silently skipped
    from the CLI path — fresh runs seeded F from REFERENCE_F and lost the §B.2
    sanity gate (round-3 C4 / design-compliance HIGH #1).

    NOTE: this executor does **not** currently write probe batches to the
    journal. Spec §B.2 calls for "journal normally (batch_id 0..n-1)"; wiring
    probe batches through the full pending/commit/journal pipeline is a
    follow-up. The immediate value here is reinstating the probe + sanity gate.
    """

    def _execute(verb: str, tx_count: int) -> tuple[StateObservation, StateObservation]:
        # Budget = tx_count × reference-F byte-rate (approx). Dispatch ignores
        # exact sizing and packs until the adapter fills the budget.
        ref = reference_f.per_scenario(verb)
        deadline_bytes = max(
            1,
            int(abs(ref["accounts"]) + abs(ref["storage"]) + abs(ref["code"])) * tx_count,
        )
        pre = sensor.read()
        dispatched = dispatch(verb, deadline_bytes, facade_ctx)
        rpc.testing_commit_block_v1([tx.rlp for tx in dispatched])
        post = sensor.read(expected_block=pre.block_number + 1)
        return pre, post

    return _execute


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

    # Single-writer guarantee: refuse to start if another orchestrator already
    # holds the state-dir lock (review C-NO-LOCK). The lock is released only
    # after the manifest is written, so crash mid-run leaves the lock file but
    # fcntl drops the advisory lock on process exit.
    with _state_dir_lock(state_dir):
        return _run_locked(
            target=target,
            state_dir=state_dir,
            preflight_ctx=preflight_ctx,
            replay_context=replay_context,
            composition_hash=composition_hash,
            target_sha=target_sha,
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
    state_dir: Path,
    preflight_ctx: FacadeContext,
    replay_context: ReplayContext,
    composition_hash: str,
    target_sha: str,
    ref_f: ReferenceF,
    env: EnvInfo,
    max_batches: int | None,
    sensor: SensorClient,
    rpc: RpcClient,
    probe_exec: ProbeExecutor | None,
    own_deps: bool,
) -> Path:
    head_block = _fetch_head_block(rpc)
    # Compute the new session id up-front so the reconcile path can stamp the
    # synthesized record with it (skeptic F-3 fix). This also eliminates a
    # duplicate manifest read that the old _extract_last_session_id did later.
    resume_session_id = _extract_last_session_id(state_dir) + 1
    decision = resolve_startup_mode(
        state_dir,
        composition_hash,
        head_block,
        rpc=rpc,
        facade_ctx=preflight_ctx,
        replay_context=replay_context,
        resume_session_id=resume_session_id,
    )

    ctx = preflight_ctx
    if decision.mode == StartupMode.RESUME and decision.last_record is not None:
        session_id = resume_session_id
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
        # C4: on fresh start, always run the probe (spec §B.2 + §C.3 step 2).
        # Tests inject a stub via LifecycleDeps; production wires the default
        # one automatically from the live rpc+sensor+facade stack.
        active_probe = probe_exec or make_default_probe_executor(
            rpc=rpc,
            sensor=sensor,
            facade_ctx=ctx,
            reference_f=ref_f,
        )
        results = run_probe(ref_f, target.qp_scenarios, active_probe)
        seed_state_from_probe(state, results)
        batch_id = len(results)

    controller = Controller(state)

    journal_path = state_dir / JOURNAL_FILENAME
    payload_path = state_dir / PAYLOAD_FILENAME
    manifest_path = state_dir / MANIFEST_FILENAME

    session_started = _now_iso()
    last_observation = sensor.read()
    stop_reason = "max_batches"

    try:
        with (
            _signal_handlers() as stop,
            JournalWriter(journal_path) as jw,
            PayloadStreamWriter(payload_path) as pw,
        ):
            completed = 0
            while not stop.is_set():
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
                    composition_hash=composition_hash,
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
    composition_hash: str,
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
            composition_hash=composition_hash,
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
        obs_diag: dict[str, Any] = controller.apply_observation(
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
            block_number=int(block["number"], 16)
            if isinstance(block.get("number"), str)
            else int(block.get("number", 0)),
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
    # H7 checkpoint: if we got here the chain is known-good end-to-end; record the
    # tail's chain_hash + batch_id so the next resume verifies only the suffix.
    checkpoint_hash = ""
    checkpoint_batch = -1
    if journal_path.exists() and journal_path.stat().st_size > 0:
        tail = JournalReader(journal_path).tail()
        if tail is not None:
            checkpoint_hash = tail.replay_core.chain_hash
            checkpoint_batch = tail.batch_id

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


def _now_iso() -> str:
    return datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


@contextlib.contextmanager
def _signal_handlers() -> Generator[threading.Event, None, None]:
    """Install SIGINT/SIGTERM handlers that set a ``threading.Event``; restore on exit.

    Previous version replaced the process-wide SIGINT handler and never restored
    it, which made pytest's own Ctrl-C trap permanently vanish for any test that
    invoked ``run()`` on the main thread. Using ``threading.Event`` instead of a
    custom flag class trims one abstraction (TRIZ simplifier #5): the loop checks
    ``stop.is_set()`` just as cheaply.
    """
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
