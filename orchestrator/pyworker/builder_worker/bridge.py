"""Pure-function bridge: BuildBatchRequest -> BuildBatchResponse."""
from __future__ import annotations

import json
import logging
from typing import Any

from orchestrator._proto import txsigner_pb2
from orchestrator.facade.context import FacadeContext
from orchestrator.facade.verbs import _VERB_BUILDERS
from ._proto import builder_pb2

log = logging.getLogger(__name__)


def build(req: builder_pb2.BuildBatchRequest) -> builder_pb2.BuildBatchResponse:
    resp = builder_pb2.BuildBatchResponse(id=req.id)
    try:
        builder_fn = _VERB_BUILDERS[req.verb]
    except KeyError:
        resp.error = f"unknown verb: {req.verb}"
        return resp

    ctx = _ctx_from_proto(req.ctx)
    for i in range(req.count):
        idx = req.start_idx + i
        try:
            signable, diag = builder_fn(ctx, idx)
        except Exception as e:  # noqa: BLE001 - convert to wire-format error
            resp.error = f"verb {req.verb} idx={idx}: {e}"
            return resp
        resp.signables.append(_signable_to_proto(signable))
        resp.diags.append(builder_pb2.BuildDiag(
            json=json.dumps(diag, sort_keys=True, separators=(",", ":")),
        ))
    resp.new_salt_cursor = ctx.salt_cursor
    return resp


def _ctx_from_proto(p: builder_pb2.FacadeCtxParams) -> FacadeContext:
    return FacadeContext(
        base_address=bytes(p.base_address),
        revision=p.revision,
        chain_id=p.chain_id,
        gas_limit=0,
        block_gas_limit=p.block_gas_limit,
        verb_gas_factors=dict(p.verb_gas_factors),
        address_stride=p.address_stride or (1 << 40),
        salt_cursor=p.salt_cursor,
    )


def _signable_to_proto(signable: dict[str, Any]) -> txsigner_pb2.TxIn:
    to_field = signable.get("to") or b""
    if isinstance(to_field, str):
        to_field = bytes.fromhex(to_field.removeprefix("0x") or "")
    elif to_field is None:
        to_field = b""

    data_field = signable.get("data") or b""
    if isinstance(data_field, str):
        data_field = bytes.fromhex(data_field.removeprefix("0x") or "")

    value = int(signable.get("value") or 0)

    return txsigner_pb2.TxIn(
        chain_id=int(signable.get("chainId", 0)),
        nonce=int(signable["nonce"]),
        gas=int(signable["gas"]),
        to=bytes(to_field),
        value=_int_to_be(value),
        data=bytes(data_field),
    )


def _int_to_be(v: int) -> bytes:
    if v == 0:
        return b""
    return v.to_bytes((v.bit_length() + 7) // 8, "big")
