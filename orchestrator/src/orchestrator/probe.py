"""Probe sequence — 100-tx batch per scenario to seed controller F."""
from __future__ import annotations

from dataclasses import dataclass
from typing import Callable, Iterable

from .controller import ControllerState
from .reference_f import AXES, ReferenceF
from .sensor import StateObservation


class CharacterizationInsufficient(Exception):
    """Raised when probe F is off by > 3× (ignoring sign for storagerefundtx)."""


PROBE_TX_COUNT = 100
SANITY_GATE_MULTIPLIER = 3.0


@dataclass
class ProbeResult:
    verb: str
    measured_per_tx: dict[str, float]


ProbeExecutor = Callable[[str, int], tuple[StateObservation, StateObservation]]
"""Signature: (verb, tx_count) -> (pre, post); caller runs the batch and returns the observations."""


def run_probe(
    reference_f: ReferenceF,
    qp_scenarios: Iterable[str],
    executor: ProbeExecutor,
    *,
    tx_count: int = PROBE_TX_COUNT,
    sanity_multiplier: float = SANITY_GATE_MULTIPLIER,
) -> list[ProbeResult]:
    """Run a 100-tx probe per scenario and return measured F values.

    `executor` is the integration seam: the caller (lifecycle.run) wires it to the facade +
    RPC + sensor stack. Tests pass a pure-Python stub.
    """
    results: list[ProbeResult] = []
    for verb in qp_scenarios:
        pre, post = executor(verb, tx_count)
        measured = {
            "accounts": (post.account_bytes - pre.account_bytes) / tx_count,
            "storage": (post.storage_bytes - pre.storage_bytes) / tx_count,
            "code": (post.code_bytes - pre.code_bytes) / tx_count,
        }
        _sanity_gate(verb, measured, reference_f, sanity_multiplier)
        results.append(ProbeResult(verb=verb, measured_per_tx=measured))
    return results


def seed_state_from_probe(state: ControllerState, results: Iterable[ProbeResult]) -> None:
    """Replace state.F entries with measured values from the probe."""
    for result in results:
        state.F[result.verb] = dict(result.measured_per_tx)


def _sanity_gate(
    verb: str,
    measured: dict[str, float],
    reference_f: ReferenceF,
    multiplier: float,
) -> None:
    ref = reference_f.per_scenario(verb)
    # For storagerefundtx the storage axis is expected to be negative; comparing on
    # absolute magnitudes avoids a spurious "10× off" alarm from a sign flip.
    for axis in AXES:
        ref_val = ref[axis]
        meas_val = measured[axis]
        if verb == "storagerefundtx" and axis == "storage":
            ref_val = abs(ref_val)
            meas_val = abs(meas_val)
        if ref_val == 0 and abs(meas_val) > multiplier:
            # Reference says "no effect" but we saw >3 B/tx — suspicious.
            raise CharacterizationInsufficient(
                f"{verb}.{axis}: measured {measured[axis]:.2f} vs reference 0"
            )
        if ref_val == 0:
            continue
        ratio = abs(meas_val) / max(abs(ref_val), 1e-6)
        if ratio > multiplier or ratio < 1.0 / multiplier:
            raise CharacterizationInsufficient(
                f"{verb}.{axis}: measured {measured[axis]:.2f} vs reference {ref[axis]:.2f} "
                f"(ratio {ratio:.2f})"
            )
