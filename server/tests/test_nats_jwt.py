import base64
import json

import nkeys

from openrmm.natsio.jwt import decode_jwt_payload, encode_nats_jwt, generate_account_keypair


def test_generated_seed_loads_in_nkeys_and_public_matches():
    seed, public = generate_account_keypair()
    assert seed.startswith("SA")
    assert public.startswith("A")
    kp = nkeys.from_seed(seed.encode())
    assert kp.public_key.decode() == public


def test_jwt_signature_verifies():
    seed, public = generate_account_keypair()
    claims = {"iss": public, "sub": "UTEST", "nats": {"type": "user", "version": 2}}
    token = encode_nats_jwt(claims, seed)

    header_b64, payload_b64, sig_b64 = token.split(".")
    header = json.loads(base64.urlsafe_b64decode(header_b64 + "=" * (-len(header_b64) % 4)))
    assert header == {"typ": "JWT", "alg": "ed25519-nkey"}

    kp = nkeys.from_seed(seed.encode())
    sig = base64.urlsafe_b64decode(sig_b64 + "=" * (-len(sig_b64) % 4))
    # nkeys.KeyPair.verify raises on mismatch
    assert kp.verify(f"{header_b64}.{payload_b64}".encode(), sig)

    assert decode_jwt_payload(token.encode())["sub"] == "UTEST"
