import os
import time
import uuid


def uuid7() -> uuid.UUID:
    """UUIDv7 (RFC 9562): 48-bit unix-ms timestamp + random. Sortable by creation time.

    stdlib gains uuid.uuid7 in 3.14; drop this then.
    """
    ts_ms = time.time_ns() // 1_000_000
    rand = os.urandom(10)
    b = ts_ms.to_bytes(6, "big") + rand
    ba = bytearray(b)
    ba[6] = (ba[6] & 0x0F) | 0x70  # version 7
    ba[8] = (ba[8] & 0x3F) | 0x80  # variant 10
    return uuid.UUID(bytes=bytes(ba))
