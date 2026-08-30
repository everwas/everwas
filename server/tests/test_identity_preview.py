"""What a network-identity policy change would do, shown before it is made.

The number this produces is the one somebody decides on, so the ways it can be
wrong are the ways somebody gets misled: too high and a safe change looks
dangerous, too low and a fleet goes quiet a month later.
"""

import datetime as dt

import pytest

from everwas.db.engine import session_scope
from everwas.models.device import Device, DeviceStatus, OsFamily
from everwas.services.identity_preview import devices_in_scope, preview_mode
from everwas.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")


async def _device(
    hostname: str,
    *,
    serial: str | None = None,
    expires_in_days: int | None = None,
    status: DeviceStatus = DeviceStatus.active,
):
    async with session_scope() as db:
        d = Device(
            id=uuid7(),
            hostname=hostname,
            os_family=OsFamily.windows,
            tags=[],
            status=status,
        )
        if serial:
            d.reported_cert_serial = serial
            d.reported_cert_at = dt.datetime.now(dt.UTC)
            if expires_in_days is not None:
                d.reported_cert_not_after = dt.datetime.now(dt.UTC) + dt.timedelta(
                    days=expires_in_days
                )
        db.add(d)
        await db.flush()
        return d.id


async def _preview(mode: str):
    async with session_scope() as db:
        return await preview_mode(db, mode, devices_in_scope())


async def test_never_names_every_machine_it_would_take_offline():
    await _device("using-ours-a", serial="aaa", expires_in_days=30)
    await _device("using-ours-b", serial="bbb", expires_in_days=10)

    p = await _preview("never")
    assert not p.safe
    assert len(p.affected) == 2
    # Soonest first: the first machine to go is the one somebody will be asked
    # about, and a list in arbitrary order buries it.
    assert [a.hostname for a in p.affected] == ["using-ours-b", "using-ours-a"]


async def test_the_window_is_reported_from_the_certificates_themselves():
    await _device("soon", serial="s", expires_in_days=5)
    await _device("later", serial="l", expires_in_days=90)

    p = await _preview("never")
    span = (p.latest_loss - p.earliest_loss).days
    assert 84 <= span <= 86, (
        "the reported window should span the real certificates, since 'these "
        "machines drop off over the next three months' reads very differently "
        "from 'these machines drop off next week'"
    )


async def test_a_machine_holding_nothing_is_not_counted_as_a_casualty():
    # Either the agent is too old to report, or the machine genuinely holds no
    # certificate. Neither loses access, and counting them would inflate the
    # number somebody is about to weigh a decision on.
    await _device("reports-nothing")
    await _device("using-ours", serial="aaa", expires_in_days=30)

    p = await _preview("never")
    assert [a.hostname for a in p.affected] == ["using-ours"]
    assert p.unaffected == 1


async def test_a_serial_with_no_expiry_is_not_guessed_at():
    # A half-reported certificate: we know it holds something and not when that
    # stops working. Inventing a date would put a number in front of somebody
    # that nothing supports.
    async with session_scope() as db:
        d = Device(
            id=uuid7(), hostname="half-reported", os_family=OsFamily.windows,
            tags=[], status=DeviceStatus.active,
        )
        d.reported_cert_serial = "abc"
        d.reported_cert_at = dt.datetime.now(dt.UTC)
        db.add(d)
        await db.flush()

    p = await _preview("never")
    assert p.affected == []
    assert p.unaffected == 1


async def test_auto_and_always_take_nothing_offline():
    # The agent's rule is that detection may never stop it providing for a
    # machine it already provides for, so neither mode can strand anybody. If
    # this ever stops being true, the preview would be quietly reassuring about
    # a change that is not safe.
    await _device("using-ours", serial="aaa", expires_in_days=30)

    for mode in ("auto", "always"):
        p = await _preview(mode)
        assert p.safe, f"{mode} reported casualties"
        assert p.affected == []
        assert p.unaffected == 1


async def test_retired_devices_are_not_counted_as_casualties():
    # They are meant to stop working. Counting them pads the number somebody is
    # weighing, and the padding is invisible.
    await _device("retired-one", serial="rrr", expires_in_days=5, status=DeviceStatus.retired)
    await _device("live-one", serial="lll", expires_in_days=5)

    p = await _preview("never")
    assert [a.hostname for a in p.affected] == ["live-one"]


async def test_a_fleet_with_nothing_at_stake_reports_safe():
    await _device("nothing-here")
    p = await _preview("never")
    assert p.safe
    assert p.earliest_loss is None


async def test_the_preview_endpoint_is_reachable_and_changes_nothing(client):
    # Registered before /{device_id}, which would otherwise parse
    # "network-identity" as a device id and 422 forever while looking correct.
    device_id = await _device("api-host", serial="aaa", expires_in_days=20)

    r = await client.get("/api/v1/devices/network-identity/preview", params={"mode": "never"})
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["affected_count"] == 1
    assert body["safe"] is False

    # Nothing was mutated: the device is exactly as it was.
    async with session_scope() as db:
        d = await db.get(Device, device_id)
        assert d.reported_cert_serial == "aaa"
        assert d.status is DeviceStatus.active


async def test_the_endpoint_refuses_a_mode_it_does_not_know(client):
    # A typo must not silently preview a different mode from the one about to
    # be applied, which would be a preview that reassures about the wrong thing.
    r = await client.get("/api/v1/devices/network-identity/preview", params={"mode": "nevr"})
    assert r.status_code == 422
