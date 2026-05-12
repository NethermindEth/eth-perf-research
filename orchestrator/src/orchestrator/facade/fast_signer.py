"""Out-of-process EIP-1559 transaction signer driven over stdio with Protobuf.

Replaces :py:meth:`eth_account.signers.local.LocalAccount.sign_transaction` for the
hot dispatcher loop. A long-lived Go subprocess (``tx-signer``) receives batches
of unsigned transaction templates and returns raw signed RLP bytes. The Go side
fans out across goroutines, so a single ``sign_batch`` call sustains hundreds of
thousands of signatures per second on a multi-core host.

Wire protocol: ``[4-byte big-endian length][protobuf SignRequest/SignResponse]``.

The class is thread-safe (one in-flight request at a time, guarded by a lock).
"""

from __future__ import annotations

import atexit
import os
import struct
import subprocess
import threading
from typing import Sequence

from orchestrator._proto import txsigner_pb2 as pb


class FastSignerError(RuntimeError):
    """Raised when the signer subprocess returns errors or dies unexpectedly."""


class FastSigner:
    """Long-lived stdio signer that batches EIP-1559 transactions through a Go subprocess."""

    def __init__(
        self,
        private_key: bytes,
        signer_binary: str | None = None,
    ) -> None:
        if len(private_key) != 32:
            raise ValueError(f"private_key must be 32 bytes, got {len(private_key)}")
        binary = signer_binary or os.environ.get("TX_SIGNER_BINARY", "/signer/tx-signer")
        env = os.environ.copy()
        env["SIGNER_PRIVATE_KEY"] = "0x" + private_key.hex()
        self._proc = subprocess.Popen(
            [binary],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
            bufsize=1 << 20,
        )
        self._lock = threading.Lock()
        self._next_id = 0
        atexit.register(self._terminate)

    def _terminate(self) -> None:
        if self._proc.poll() is None:
            try:
                self._proc.stdin.close()
            except Exception:
                pass
            try:
                self._proc.terminate()
                self._proc.wait(timeout=2)
            except Exception:
                self._proc.kill()

    @staticmethod
    def _int_to_be(value: int) -> bytes:
        """Big-endian, minimal-length encoding (matches Ethereum convention)."""
        v = int(value)
        if v == 0:
            return b""
        return v.to_bytes((v.bit_length() + 7) // 8, "big")

    @staticmethod
    def _hex_to_bytes(value) -> bytes:
        if not value:
            return b""
        if isinstance(value, bytes):
            return value
        if isinstance(value, str):
            return bytes.fromhex(value.removeprefix("0x"))
        raise TypeError(f"expected hex str or bytes, got {type(value).__name__}")

    def sign_batch(self, tx_dicts: Sequence[dict]) -> list[bytes]:
        """Sign a batch of EIP-1559 type-2 tx templates.

        Each ``tx_dicts`` entry is a dict with the same keys
        ``eth_account.Account.sign_transaction`` accepts (``chainId``, ``nonce``,
        ``maxPriorityFeePerGas``, ``maxFeePerGas``, ``gas``, ``to``, ``value``,
        ``data``, ``accessList``). Returns a list of raw signed RLP bytes (with
        the ``0x02`` type prefix) in the same order as the input.

        Raises :class:`FastSignerError` if any signature fails or the subprocess
        terminates.
        """
        if not tx_dicts:
            return []

        with self._lock:
            self._next_id += 1
            req = pb.SignRequest(id=self._next_id)
            for t in tx_dicts:
                pb_tx = req.txs.add()
                pb_tx.chain_id = int(t.get("chainId", 1))
                pb_tx.nonce = int(t["nonce"])
                pb_tx.max_priority_fee_per_gas = self._int_to_be(t["maxPriorityFeePerGas"])
                pb_tx.max_fee_per_gas = self._int_to_be(t["maxFeePerGas"])
                pb_tx.gas = int(t["gas"])
                pb_tx.to = self._hex_to_bytes(t.get("to"))
                pb_tx.value = self._int_to_be(t.get("value", 0))
                pb_tx.data = self._hex_to_bytes(t.get("data"))
                for entry in t.get("accessList", []) or []:
                    pb_e = pb_tx.access_list.add()
                    pb_e.address = self._hex_to_bytes(entry["address"])
                    pb_e.storage_keys.extend(
                        self._hex_to_bytes(k) for k in entry.get("storageKeys", [])
                    )

            body = req.SerializeToString()
            try:
                self._proc.stdin.write(struct.pack(">I", len(body)))
                self._proc.stdin.write(body)
                self._proc.stdin.flush()
            except (BrokenPipeError, OSError) as exc:
                stderr_tail = self._read_stderr_tail()
                raise FastSignerError(
                    f"signer write failed: {exc}; stderr tail: {stderr_tail!r}"
                ) from exc

            length_bytes = self._read_exact(4)
            (resp_len,) = struct.unpack(">I", length_bytes)
            resp_body = self._read_exact(resp_len)

        resp = pb.SignResponse()
        resp.ParseFromString(resp_body)
        if resp.errors:
            raise FastSignerError(f"signer errors: {list(resp.errors)}")
        if len(resp.raw) != len(tx_dicts):
            raise FastSignerError(
                f"signer returned {len(resp.raw)} results for {len(tx_dicts)} inputs"
            )
        return [bytes(r) for r in resp.raw]

    def _read_exact(self, n: int) -> bytes:
        """Read exactly *n* bytes from the signer's stdout or raise."""
        buf = bytearray()
        while len(buf) < n:
            chunk = self._proc.stdout.read(n - len(buf))
            if not chunk:
                stderr_tail = self._read_stderr_tail()
                raise FastSignerError(
                    f"signer subprocess closed stdout after {len(buf)}/{n} bytes; "
                    f"stderr tail: {stderr_tail!r}"
                )
            buf.extend(chunk)
        return bytes(buf)

    def _read_stderr_tail(self, max_bytes: int = 4096) -> bytes:
        """Best-effort drain of the signer's stderr (used for error diagnostics)."""
        try:
            return os.read(self._proc.stderr.fileno(), max_bytes)
        except Exception:
            return b""
