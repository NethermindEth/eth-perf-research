from orchestrator._proto import txsigner_pb2 as _txsigner_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class FacadeCtxParams(_message.Message):
    __slots__ = ("base_address", "revision", "chain_id", "block_gas_limit", "salt_cursor", "verb_gas_factors", "address_stride")
    class VerbGasFactorsEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: float
        def __init__(self, key: _Optional[str] = ..., value: _Optional[float] = ...) -> None: ...
    BASE_ADDRESS_FIELD_NUMBER: _ClassVar[int]
    REVISION_FIELD_NUMBER: _ClassVar[int]
    CHAIN_ID_FIELD_NUMBER: _ClassVar[int]
    BLOCK_GAS_LIMIT_FIELD_NUMBER: _ClassVar[int]
    SALT_CURSOR_FIELD_NUMBER: _ClassVar[int]
    VERB_GAS_FACTORS_FIELD_NUMBER: _ClassVar[int]
    ADDRESS_STRIDE_FIELD_NUMBER: _ClassVar[int]
    base_address: bytes
    revision: int
    chain_id: int
    block_gas_limit: int
    salt_cursor: int
    verb_gas_factors: _containers.ScalarMap[str, float]
    address_stride: int
    def __init__(self, base_address: _Optional[bytes] = ..., revision: _Optional[int] = ..., chain_id: _Optional[int] = ..., block_gas_limit: _Optional[int] = ..., salt_cursor: _Optional[int] = ..., verb_gas_factors: _Optional[_Mapping[str, float]] = ..., address_stride: _Optional[int] = ...) -> None: ...

class BuildBatchRequest(_message.Message):
    __slots__ = ("id", "verb", "start_idx", "count", "ctx")
    ID_FIELD_NUMBER: _ClassVar[int]
    VERB_FIELD_NUMBER: _ClassVar[int]
    START_IDX_FIELD_NUMBER: _ClassVar[int]
    COUNT_FIELD_NUMBER: _ClassVar[int]
    CTX_FIELD_NUMBER: _ClassVar[int]
    id: int
    verb: str
    start_idx: int
    count: int
    ctx: FacadeCtxParams
    def __init__(self, id: _Optional[int] = ..., verb: _Optional[str] = ..., start_idx: _Optional[int] = ..., count: _Optional[int] = ..., ctx: _Optional[_Union[FacadeCtxParams, _Mapping]] = ...) -> None: ...

class BuildDiag(_message.Message):
    __slots__ = ("json",)
    JSON_FIELD_NUMBER: _ClassVar[int]
    json: str
    def __init__(self, json: _Optional[str] = ...) -> None: ...

class BuildBatchResponse(_message.Message):
    __slots__ = ("id", "signables", "diags", "error", "new_salt_cursor")
    ID_FIELD_NUMBER: _ClassVar[int]
    SIGNABLES_FIELD_NUMBER: _ClassVar[int]
    DIAGS_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    NEW_SALT_CURSOR_FIELD_NUMBER: _ClassVar[int]
    id: int
    signables: _containers.RepeatedCompositeFieldContainer[_txsigner_pb2.TxIn]
    diags: _containers.RepeatedCompositeFieldContainer[BuildDiag]
    error: str
    new_salt_cursor: int
    def __init__(self, id: _Optional[int] = ..., signables: _Optional[_Iterable[_Union[_txsigner_pb2.TxIn, _Mapping]]] = ..., diags: _Optional[_Iterable[_Union[BuildDiag, _Mapping]]] = ..., error: _Optional[str] = ..., new_salt_cursor: _Optional[int] = ...) -> None: ...
