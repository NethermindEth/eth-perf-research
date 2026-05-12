"""Build-worker subprocess. Speak length-prefixed protobuf over stdio."""
from __future__ import annotations

import logging
import struct
import sys

from .bridge import build
from ._proto import builder_pb2


def main() -> None:
    logging.basicConfig(
        level=logging.WARNING,
        format="[builder-worker] %(message)s",
        stream=sys.stderr,
    )
    stdin = sys.stdin.buffer
    stdout = sys.stdout.buffer

    while True:
        header = stdin.read(4)
        if len(header) == 0:
            return  # clean EOF
        if len(header) < 4:
            logging.error("short read on length header")
            return
        (n,) = struct.unpack(">I", header)
        body = stdin.read(n)
        if len(body) < n:
            logging.error("short read on body (want %d got %d)", n, len(body))
            return
        req = builder_pb2.BuildBatchRequest()
        req.ParseFromString(body)
        resp = build(req)
        out = resp.SerializeToString()
        stdout.write(struct.pack(">I", len(out)))
        stdout.write(out)
        stdout.flush()


if __name__ == "__main__":
    main()
