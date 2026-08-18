"""The sync surface's one pagination contract.

Every /api/v1/sync endpoint pages the same way, because a consumer that has
to remember which endpoint uses which scheme will eventually walk one of
them wrong and read the truncation as truth:

- envelope: {"items": [...], "has_more": bool, "next_cursor": str | null}.
  items is always a JSON array — an empty collection is [], never null,
  because a reconciling consumer reads null as "everything is gone".
- cursor: opaque base64url(JSON list of the last row's key values). Keyset,
  strictly advancing, so a row inserted behind the cursor is missed and one
  inserted ahead is seen — a sweep is a sweep, not a snapshot. Offset
  pagination would instead skip or duplicate rows as the table moves under
  the walk.
- termination: has_more false AND next_cursor null, together, always.
- fetch limit+1 rows, return limit: existence of another page without a
  count(*) over tables that only grow (the audit route's trick).

A cursor that does not decode is a 422, not an empty page: garbage in the
cursor means the client has a bug, and an empty 200 would bury it.
"""

import base64
import binascii
import json
from datetime import datetime

from fastapi import HTTPException, status

#: Matches the audit route's ceiling. Big enough that a 5k-device fleet is
#: 25 calls, small enough that no single response is a surprise.
DEFAULT_LIMIT = 100
MAX_LIMIT = 200


def encode_cursor(values: list[str]) -> str:
    return base64.urlsafe_b64encode(json.dumps(values).encode()).decode().rstrip("=")


def decode_cursor(raw: str, parts: int) -> list[str]:
    """Decode a client-presented cursor or refuse loudly."""
    try:
        decoded = json.loads(base64.urlsafe_b64decode(raw + "=" * (-len(raw) % 4)))
    except (binascii.Error, ValueError, UnicodeDecodeError) as exc:
        raise HTTPException(
            status.HTTP_422_UNPROCESSABLE_ENTITY,
            "cursor does not decode; pass next_cursor back verbatim or omit it",
        ) from exc
    if (
        not isinstance(decoded, list)
        or len(decoded) != parts
        or not all(isinstance(v, str) for v in decoded)
    ):
        raise HTTPException(
            status.HTTP_422_UNPROCESSABLE_ENTITY,
            "cursor does not fit this endpoint; cursors are not portable between endpoints",
        )
    return decoded


def require_aware(name: str, value: datetime | None) -> datetime | None:
    """Refuse naive datetimes on the time-travel parameters.

    The audit route learned this first: a naive timestamp is silently
    interpreted against the server clock, which answers a different question
    than the caller asked, in a way nobody notices until the answers matter.
    """
    if value is not None and value.tzinfo is None:
        raise HTTPException(
            status.HTTP_422_UNPROCESSABLE_ENTITY,
            f"{name} must carry a timezone; a naive timestamp is read against "
            "the server's clock and answers a different question",
        )
    return value
