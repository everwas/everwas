"""Agent packages, served by the server the agent is enrolled to.

Bootstrapping and remediation both need somewhere to fetch an installer from,
and that somewhere has to be reachable from wherever the machine currently is.
GitHub frequently is not. A device on a provisioning or quarantine VLAN is often
permitted to reach exactly one host, and that host is its management server: the
egress rule already exists because the agent has to phone home. So hosting the
packages here makes the download work precisely when the network is most
restricted, which is when bootstrapping and remediation actually happen.

It also closes the loop with ADR-0003. A device that has no certificate yet sits
in a provisioning VLAN, and the sequence "reach the server, install the agent,
enrol, request a certificate, get moved to the real VLAN" needs every step to be
reachable from that VLAN. A GitHub-hosted installer breaks the first one.

Public, deliberately. An installer is not a secret, and requiring a credential
to download the thing that obtains the credential is a circle. What IS secret is
the enrollment token, which is supplied separately at install time.
"""

import hashlib
import re
from pathlib import Path

import structlog
from fastapi import APIRouter, HTTPException, status
from fastapi.responses import FileResponse
from pydantic import BaseModel

from everwas.config import get_settings

router = APIRouter()
log = structlog.get_logger()

#: The only filenames this will serve, and the only ones it will list.
#:
#: An allowlist rather than "whatever is in the directory", because the
#: directory is a filesystem path an operator can drop anything into, and a
#: stray backup, key or config left there would otherwise be downloadable by
#: anyone who guessed the name. Matching the release naming also means the
#: listing can report platform, arch and version without a side-car manifest to
#: fall out of step with the files.
PACKAGE_RE = re.compile(
    r"^everwas-agent[-_](?P<version>\d{4}\.\d{2}\.\d{2}(?:\.\d+)?)"
    r"[-_](?P<platform>windows|linux|darwin)"
    r"[-_](?P<arch>amd64|arm64)"
    r"\.(?P<ext>msi|exe|deb|rpm|pkg|tar\.gz|zip)(?P<sig>\.minisig)?$"
)

#: Hashing a file per request would make the listing quadratic in package size
#: on a page that is polled. Keyed on (path, mtime, size) so a replaced file is
#: rehashed rather than served with a stale checksum, which would be worse than
#: no checksum at all: it would fail verification on a correct download.
_HASH_CACHE: dict[tuple[str, float, int], str] = {}


class PackageOut(BaseModel):
    filename: str
    version: str
    platform: str
    arch: str
    size: int
    #: Whether a detached minisign signature is present beside the file.
    signed: bool = False
    #: Present so a downloader can verify what it received. A download with no
    #: way to check it is a download that installs whatever it was handed,
    #: which for an agent binary is remote code execution by mistake.
    sha256: str


def _packages_dir() -> Path:
    return Path(get_settings().packages_dir)


def _sha256(path: Path) -> str:
    stat = path.stat()
    key = (str(path), stat.st_mtime, stat.st_size)
    cached = _HASH_CACHE.get(key)
    if cached is not None:
        return cached
    digest = hashlib.sha256()
    with path.open("rb") as fh:
        for chunk in iter(lambda: fh.read(1024 * 1024), b""):
            digest.update(chunk)
    _HASH_CACHE[key] = digest.hexdigest()
    return _HASH_CACHE[key]


@router.get("")
async def list_packages() -> list[PackageOut]:
    """Every agent package this server can hand out.

    An unconfigured or empty directory is an empty list, not an error. A server
    with nothing uploaded yet is an ordinary state, and a 500 here would make
    the console's download page look broken on every fresh install, which
    teaches operators to ignore it.
    """
    directory = _packages_dir()
    if not directory.is_dir():
        return []

    out: list[PackageOut] = []
    for path in sorted(directory.iterdir()):
        if not path.is_file():
            continue
        match = PACKAGE_RE.match(path.name)
        if match is None:
            continue
        if match["sig"]:
            # Signatures are downloadable but not listed as packages. Listing
            # them would put "everwas-agent_X.msi.minisig" in a picker next to
            # the thing it signs, which is an easy click away from dispatching
            # a signature as if it were an installer.
            continue
        out.append(
            PackageOut(
                filename=path.name,
                # Whether the detached signature is sitting beside it. An
                # update cannot be dispatched without one, so an operator
                # needs to see which packages are actually deployable rather
                # than discovering it when the job is refused.
                signed=(directory / f"{path.name}.minisig").is_file(),
                version=match["version"],
                platform=match["platform"],
                arch=match["arch"],
                size=path.stat().st_size,
                sha256=_sha256(path),
            )
        )
    return out


@router.get("/{filename}")
async def download_package(filename: str) -> FileResponse:
    """Serve one package by name.

    The name is caller-supplied, so it is attacker-controlled input. Two
    independent guards, because either alone has been enough to fail elsewhere:
    the name must match the release pattern, and the resolved path must still
    be inside the package directory afterwards. The second is what catches
    traversal that survives the first, and resolving-then-containing is the
    check that works rather than filtering the string for "..".
    """
    if PACKAGE_RE.match(filename) is None:
        # 404 rather than 400: a caller probing for files should not learn the
        # difference between "not a package name" and "not here".
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown package")

    directory = _packages_dir().resolve()
    path = (directory / filename).resolve()
    if not path.is_relative_to(directory) or not path.is_file():
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown package")

    return FileResponse(
        path,
        media_type="application/octet-stream",
        filename=filename,
        headers={
            # The checksum, so a client that streams straight to disk can
            # verify without a second request to the manifest.
            "X-Everwas-SHA256": _sha256(path),
        },
    )
