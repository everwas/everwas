"""The server distributes the agent packages for the fleet it manages.

Bootstrapping and remediation both need a place to get the installer that is
reachable from wherever the machine currently is. GitHub often is not: a device
on a provisioning or quarantine VLAN is frequently allowed to reach exactly one
thing, and that thing is the management server it is enrolled to. Hosting the
packages here means the download works precisely when the network is most
restricted, which is when bootstrapping and remediation actually happen.

The hazards are the ordinary ones for serving files off disk, and the first is
path traversal.
"""

import hashlib

import pytest

pytestmark = pytest.mark.usefixtures("pg_database")


@pytest.fixture
def packages(tmp_path, monkeypatch):
    """A package directory with one real file in it."""
    from everwas.config import get_settings

    d = tmp_path / "packages"
    d.mkdir()
    (d / "everwas-agent_2026.08.18_windows_amd64.msi").write_bytes(b"MSI-CONTENT")
    (d / "everwas-agent_2026.08.18_linux_amd64.deb").write_bytes(b"DEB-CONTENT")
    # Something that is not a package, to prove the listing is an allowlist
    # rather than "whatever is in the directory".
    (d / "notes.txt").write_bytes(b"not a package")

    get_settings.cache_clear()
    monkeypatch.setenv("EVERWAS_PACKAGES_DIR", str(d))
    yield d
    get_settings.cache_clear()


async def test_the_manifest_lists_packages_with_checksums(client, packages):
    r = await client.get("/api/v1/packages")
    assert r.status_code == 200
    items = {p["filename"]: p for p in r.json()}

    assert "everwas-agent_2026.08.18_windows_amd64.msi" in items
    entry = items["everwas-agent_2026.08.18_windows_amd64.msi"]
    # The checksum travels with the listing: a downloader that cannot verify
    # what it fetched is one that installs whatever it was handed.
    assert entry["sha256"] == hashlib.sha256(b"MSI-CONTENT").hexdigest()
    assert entry["size"] == len(b"MSI-CONTENT")
    assert entry["platform"] == "windows"
    assert entry["arch"] == "amd64"
    assert entry["version"] == "2026.08.18"


async def test_files_that_are_not_packages_are_not_listed(client, packages):
    r = await client.get("/api/v1/packages")
    assert "notes.txt" not in {p["filename"] for p in r.json()}


async def test_a_package_can_be_downloaded(client, packages):
    r = await client.get("/api/v1/packages/everwas-agent_2026.08.18_linux_amd64.deb")
    assert r.status_code == 200
    assert r.content == b"DEB-CONTENT"


@pytest.mark.parametrize(
    "attempt",
    [
        "../../../../etc/passwd",
        "..%2f..%2fetc%2fpasswd",
        "/etc/passwd",
        "everwas-agent/../../etc/passwd",
    ],
)
async def test_path_traversal_is_refused(client, packages, attempt):
    """The filename comes from the caller, so it is attacker-controlled input.

    The shell recording endpoint learned this already; the fix there was to
    resolve and then check containment rather than to filter the string, and
    the same applies here.
    """
    r = await client.get(f"/api/v1/packages/{attempt}")
    assert r.status_code in (400, 404), f"{attempt!r} was not refused"
    assert b"root:" not in r.content


async def test_a_file_outside_the_allowlist_cannot_be_fetched(client, packages):
    """Listing is an allowlist, and so is fetching. Otherwise the directory
    becomes an arbitrary file server for anything an operator drops in it."""
    r = await client.get("/api/v1/packages/notes.txt")
    assert r.status_code == 404


async def test_a_missing_package_is_a_clean_404(client, packages):
    r = await client.get("/api/v1/packages/everwas-agent_9999.99.99_windows_amd64.msi")
    assert r.status_code == 404


async def test_an_unconfigured_directory_is_an_empty_list_not_a_crash(
    client, tmp_path, monkeypatch
):
    """A server with no packages uploaded yet is a normal state, not an error.

    Returning 500 here would make the console's download page look broken on
    every fresh install, which teaches operators to ignore it.
    """
    from everwas.config import get_settings

    get_settings.cache_clear()
    monkeypatch.setenv("EVERWAS_PACKAGES_DIR", str(tmp_path / "does-not-exist"))
    r = await client.get("/api/v1/packages")
    get_settings.cache_clear()
    assert r.status_code == 200
    assert r.json() == []


# --- dispatching an update from a hosted package -----------------------------


async def test_a_signature_is_downloadable_but_not_listed(client, packages):
    """Both halves of an update come from the one host a device can reach.

    Not listed, though: a signature sitting in a package picker next to the
    thing it signs is one click away from being dispatched as if it were the
    installer.
    """
    (packages / "everwas-agent_2026.08.18_linux_amd64.deb.minisig").write_bytes(b"SIG")

    listed = {p["filename"] for p in (await client.get("/api/v1/packages")).json()}
    assert "everwas-agent_2026.08.18_linux_amd64.deb.minisig" not in listed

    r = await client.get("/api/v1/packages/everwas-agent_2026.08.18_linux_amd64.deb.minisig")
    assert r.status_code == 200
    assert r.content == b"SIG"


async def test_the_listing_says_which_packages_are_deployable(client, packages):
    """An update cannot be dispatched without a signature, so an operator has
    to see which packages have one BEFORE choosing, not discover it when the
    job is refused."""
    (packages / "everwas-agent_2026.08.18_linux_amd64.deb.minisig").write_bytes(b"SIG")

    items = {p["filename"]: p for p in (await client.get("/api/v1/packages")).json()}
    assert items["everwas-agent_2026.08.18_linux_amd64.deb"]["signed"] is True
    assert items["everwas-agent_2026.08.18_windows_amd64.msi"]["signed"] is False
