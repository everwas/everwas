"""Minimal NATS JWT (v2) encoding for the auth-callout responder.

Wire format verified against nats-io/jwt v2 and nats-server auth_callout.go:
- header {"typ":"JWT","alg":"ed25519-nkey"}
- base64url WITHOUT padding for all three segments
- signature: ed25519 over the ASCII bytes of "header.payload"
- jti is written by the Go lib but never validated by nats-server; we set a
  random one for log correlation only
"""

import base64
import json
import os
import uuid

import nkeys

_HEADER = base64.urlsafe_b64encode(
    json.dumps({"typ": "JWT", "alg": "ed25519-nkey"}, separators=(",", ":")).encode()
).rstrip(b"=")


def encode_nats_jwt(claims: dict, seed: str) -> str:
    """Sign claims with an nkeys seed (SA... for accounts). iss must already be set."""
    kp = nkeys.from_seed(seed.encode())
    try:
        payload = base64.urlsafe_b64encode(
            json.dumps(claims, separators=(",", ":")).encode()
        ).rstrip(b"=")
        to_sign = _HEADER + b"." + payload
        sig = base64.urlsafe_b64encode(kp.sign(to_sign)).rstrip(b"=")
        return (to_sign + b"." + sig).decode()
    finally:
        kp.wipe()


def decode_jwt_payload(token: bytes) -> dict:
    """Decode the claims segment of a JWT without verifying the signature.

    The auth-callout request is produced by our own nats-server over a private
    connection; the protocol does not require responders to verify it.
    """
    parts = token.split(b".")
    if len(parts) != 3:
        raise ValueError("not a JWT")
    payload = parts[1]
    payload += b"=" * (-len(payload) % 4)
    return json.loads(base64.urlsafe_b64decode(payload))


def new_jti() -> str:
    return uuid.uuid4().hex


# --- account keypair generation (the nkeys package can only load seeds) ---

_PREFIX_SEED = 18 << 3  # 'S'
_PREFIX_ACCOUNT = 0  # 'A'
_B32 = base64.b32encode


def _crc16_xmodem(data: bytes) -> int:
    crc = 0
    for byte in data:
        crc ^= byte << 8
        for _ in range(8):
            crc = ((crc << 1) ^ 0x1021 if crc & 0x8000 else crc << 1) & 0xFFFF
    return crc


def _b32_nopad(raw: bytes) -> str:
    return _B32(raw).decode().rstrip("=")


def generate_account_keypair() -> tuple[str, str]:
    """Return (seed 'SA...', public key 'A...') for a fresh account nkey."""
    from nacl.signing import SigningKey

    raw_seed = os.urandom(32)
    public_raw = bytes(SigningKey(raw_seed).verify_key)

    b1 = _PREFIX_SEED | (_PREFIX_ACCOUNT >> 5)
    b2 = (_PREFIX_ACCOUNT & 0x1F) << 3
    seed_body = bytes([b1, b2]) + raw_seed
    seed = _b32_nopad(seed_body + _crc16_xmodem(seed_body).to_bytes(2, "little"))

    pub_body = bytes([_PREFIX_ACCOUNT]) + public_raw
    public = _b32_nopad(pub_body + _crc16_xmodem(pub_body).to_bytes(2, "little"))
    return seed, public
