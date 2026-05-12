from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class AccessTuple(_message.Message):
    __slots__ = ("address", "storage_keys")
    ADDRESS_FIELD_NUMBER: _ClassVar[int]
    STORAGE_KEYS_FIELD_NUMBER: _ClassVar[int]
    address: bytes
    storage_keys: _containers.RepeatedScalarFieldContainer[bytes]
    def __init__(self, address: _Optional[bytes] = ..., storage_keys: _Optional[_Iterable[bytes]] = ...) -> None: ...

class TxIn(_message.Message):
    __slots__ = ("chain_id", "nonce", "max_priority_fee_per_gas", "max_fee_per_gas", "gas", "to", "value", "data", "access_list")
    CHAIN_ID_FIELD_NUMBER: _ClassVar[int]
    NONCE_FIELD_NUMBER: _ClassVar[int]
    MAX_PRIORITY_FEE_PER_GAS_FIELD_NUMBER: _ClassVar[int]
    MAX_FEE_PER_GAS_FIELD_NUMBER: _ClassVar[int]
    GAS_FIELD_NUMBER: _ClassVar[int]
    TO_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    ACCESS_LIST_FIELD_NUMBER: _ClassVar[int]
    chain_id: int
    nonce: int
    max_priority_fee_per_gas: bytes
    max_fee_per_gas: bytes
    gas: int
    to: bytes
    value: bytes
    data: bytes
    access_list: _containers.RepeatedCompositeFieldContainer[AccessTuple]
    def __init__(self, chain_id: _Optional[int] = ..., nonce: _Optional[int] = ..., max_priority_fee_per_gas: _Optional[bytes] = ..., max_fee_per_gas: _Optional[bytes] = ..., gas: _Optional[int] = ..., to: _Optional[bytes] = ..., value: _Optional[bytes] = ..., data: _Optional[bytes] = ..., access_list: _Optional[_Iterable[_Union[AccessTuple, _Mapping]]] = ...) -> None: ...

class SignRequest(_message.Message):
    __slots__ = ("id", "txs")
    ID_FIELD_NUMBER: _ClassVar[int]
    TXS_FIELD_NUMBER: _ClassVar[int]
    id: int
    txs: _containers.RepeatedCompositeFieldContainer[TxIn]
    def __init__(self, id: _Optional[int] = ..., txs: _Optional[_Iterable[_Union[TxIn, _Mapping]]] = ...) -> None: ...

class SignResponse(_message.Message):
    __slots__ = ("id", "raw", "errors")
    ID_FIELD_NUMBER: _ClassVar[int]
    RAW_FIELD_NUMBER: _ClassVar[int]
    ERRORS_FIELD_NUMBER: _ClassVar[int]
    id: int
    raw: _containers.RepeatedScalarFieldContainer[bytes]
    errors: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, id: _Optional[int] = ..., raw: _Optional[_Iterable[bytes]] = ..., errors: _Optional[_Iterable[str]] = ...) -> None: ...
