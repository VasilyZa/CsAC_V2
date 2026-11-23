#!/usr/bin/env python3
"""Receive CI artifacts through the fixed SSH commands for the two deploy keys."""

import fcntl
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

CONFIG_DIR = Path("/etc/csac-ci")
STATE_DIR = Path("/var/lib/csac-ci")
MIB = 1024**2
LIMITS = {"backend": 256 * MIB, "web": 32 * MIB}
HEADER = re.compile(rb"release ([0-9a-f]{40}) ([1-9][0-9]{0,9}) ([1-9][0-9]{0,9}) ([0-9a-f]{64})\n")


class ReleaseError(Exception):
    pass


def atomic_write(path, content, mode=0o644):
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as output:
            os.fchmod(output.fileno(), mode)
            output.write(content)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        Path(temporary).unlink(missing_ok=True)


def run(arguments, operation, timeout=120):
    try:
        result = subprocess.run(arguments, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                timeout=timeout, check=True)
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        raise ReleaseError(f"{operation} failed") from error
    return result.stdout


def receive(stream, path, size, expected):
    digest = hashlib.sha256()
    with path.open("wb") as output:
        while size:
            block = stream.read(min(size, MIB))
            if not block:
                raise ReleaseError("Upload ended before the declared size")
            size -= len(block)
            digest.update(block)
            output.write(block)
    if stream.read(1) or digest.hexdigest() != expected:
        raise ReleaseError("Upload size or checksum does not match")


def probe(config, path):
    return run(["curl", "--fail", "--silent", "--noproxy", "*", "--max-time", "12",
                "--resolve", config["probe_resolve"], config["probe_base"] + path], "1Panel origin check", 16)


def deploy_backend(config, archive, revision, build):
    compose_file = Path(config["compose_file"])
    original = compose_file.read_bytes()
    mode = compose_file.stat().st_mode & 0o777
    image = f"csac-v2-local:{revision}-{build}"
    image_line = rb"^([ \t]+image:[ \t]+)csac-v2-local:[0-9a-zA-Z_.-]+([ \t]*)$"
    updated, count = re.subn(image_line, lambda match: match[1] + image.encode() + match[2], original, flags=re.M)
    if count != 1:
        raise ReleaseError("Expected one CsAC image in the production Compose file")
    target = ["docker", "compose", "--project-directory", str(compose_file.parent), "-f", str(compose_file)]
    start = [*target, "up", "-d", "--no-deps", "--wait", "--wait-timeout", "75", config["service"]]
    run(["docker", "image", "load", "--input", str(archive)], "Docker image import")
    inspection = run(["docker", "image", "inspect", "--format",
                      '{{.Os}}/{{.Architecture}} {{index .Config.Labels "org.opencontainers.image.revision"}}',
                      image], "Docker image inspection")
    if inspection.strip() != f"linux/amd64 {revision}".encode():
        raise ReleaseError("Docker image metadata does not match the release")
    try:
        atomic_write(compose_file, updated, mode)
        run(start, "Compose service readiness")
        probe(config, "/readyz")
    except (OSError, ReleaseError) as error:
        atomic_write(compose_file, original, mode)
        run(start, "Restoring the previous backend")
        raise ReleaseError(f"{error}. Restored the previous backend") from error


def unpack_web(archive, directory):
    unpacked_size = 0
    with tarfile.open(archive, mode="r|gz") as release:
        for member in release:
            name = PurePosixPath(member.name)
            if name.is_absolute() or ".." in name.parts or not name.parts:
                raise ReleaseError("Invalid path in web artifact")
            if name.parts[0] == "assets" and member.isdir():
                continue
            if not member.isfile() or (name != PurePosixPath("index.html") and
                                       (name.parts[0] != "assets" or len(name.parts) < 2)):
                raise ReleaseError("Unexpected item in web artifact")
            unpacked_size += member.size
            if unpacked_size > 128 * MIB:
                raise ReleaseError("Expanded web artifact exceeds its size limit")
            target = directory.joinpath(*name.parts)
            target.parent.mkdir(parents=True, exist_ok=True)
            with release.extractfile(member) as source, target.open("xb") as output:
                shutil.copyfileobj(source, output)
    if not (directory / "index.html").is_file() or not (directory / "assets").is_dir():
        raise ReleaseError("Web artifact must contain an index and assets")


def deploy_web(config, archive, directory):
    directory.mkdir()
    unpack_web(archive, directory)
    root = Path(config["document_root"])
    index = root / "index.html"
    old_index = index.read_bytes()
    new_index = (directory / "index.html").read_bytes()
    for source in (directory / "assets").rglob("*"):
        if not source.is_file():
            continue
        destination = root / source.relative_to(directory)
        destination.parent.mkdir(parents=True, exist_ok=True)
        content = source.read_bytes()
        if destination.exists():
            if destination.read_bytes() != content:
                raise ReleaseError("A published asset has different content under the same filename")
        else:
            atomic_write(destination, content)
    try:
        atomic_write(index, new_index)
        if probe(config, "/index.html") != new_index:
            raise ReleaseError("The served index does not match the new build")
    except (OSError, ReleaseError) as error:
        atomic_write(index, old_index)
        raise ReleaseError(f"{error}. Restored the previous index") from error


def main(kind, stream):
    header = stream.readline(256)
    if header == b"probe\n":
        print(f"CsAC {kind} deployment ready")
        return
    match = HEADER.fullmatch(header)
    if not match:
        raise ReleaseError("Invalid release header")
    revision, number, size, expected = (part.decode("ascii") for part in match.groups())
    if int(size) > LIMITS[kind]:
        raise ReleaseError("Artifact exceeds its size limit")
    config = json.loads((CONFIG_DIR / f"{kind}.json").read_text())
    state = STATE_DIR / f"{kind}.run"
    with tempfile.TemporaryDirectory(dir=STATE_DIR) as scratch:
        directory = Path(scratch)
        archive = directory / "release.tar.gz"
        receive(stream, archive, int(size), expected)
        with (STATE_DIR / "deploy.lock").open("a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            if state.exists() and int(number) <= int(state.read_text()):
                print("Skipped a build already deployed or superseded")
                return
            if kind == "backend":
                deploy_backend(config, archive, revision, number)
            else:
                deploy_web(config, archive, directory / "site")
            atomic_write(state, (number + "\n").encode(), 0o600)
    print(f"Deployed {kind} {revision}")


if __name__ == "__main__":
    try:
        kind = sys.argv[1]
        if kind not in LIMITS:
            raise ReleaseError("Unknown deployment target")
        main(kind, sys.stdin.buffer)
    except ReleaseError as error:
        sys.exit(str(error))
    except (OSError, tarfile.TarError, ValueError, KeyError):
        sys.exit("The deployment artifact or server configuration could not be processed")
