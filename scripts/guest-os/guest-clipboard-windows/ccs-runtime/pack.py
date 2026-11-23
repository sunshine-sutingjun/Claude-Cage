"""Pack the Windows x64 CCS runtime from the host's offline npm cache."""

import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile


def sha256(path):
    with path.open("rb") as file:
        return hashlib.file_digest(file, "sha256").hexdigest()


def add_file(archive, name, data):
    entry = tarfile.TarInfo(name)
    entry.mode = 0o644
    entry.size = len(data)
    archive.addfile(entry, io.BytesIO(data))


def pack(stage, archive):
    source = Path(__file__).resolve().parent
    for name in ("package.json", "package-lock.json"):
        # Keep checkout line endings out of both the archive and its manifest.
        (stage / name).write_bytes((source / name).read_text(encoding="utf-8").encode())
    subprocess.run(
        [
            shutil.which("npm") or "npm", "ci", "--offline", "--ignore-scripts",
            "--os=win32", "--cpu=x64", "--include=optional", "--no-bin-links",
            "--no-audit", "--no-fund",
        ],
        cwd=stage, check=True,
    )
    # npm can silently omit an optional package on a cache miss. All Windows
    # packages in this lock must be present, including optional dependencies.
    lock = json.loads((stage / "package-lock.json").read_bytes())
    for key, package in lock["packages"].items():
        if key and "win32" in package.get("os", ["win32"]):
            actual = json.loads((stage / key / "package.json").read_bytes())
            if actual["version"] != package["version"]:
                raise ValueError(f"Version differs from the lock for {key}")

    manifest = {}
    names = set()
    with tarfile.open(archive, "w", format=tarfile.PAX_FORMAT) as tar:
        for path in sorted(stage.rglob("*"), key=lambda p: p.relative_to(stage).as_posix()):
            name = path.relative_to(stage).as_posix()
            if name.casefold() in names or path.is_symlink():
                raise ValueError(f"Unexpected alias in Windows package: {name}")
            names.add(name.casefold())
            if path.is_dir() or name == "node_modules/.package-lock.json":
                continue
            if not path.is_file():
                raise ValueError(f"Unexpected package entry: {name}")
            data = path.read_bytes()
            manifest[name] = hashlib.sha256(data).hexdigest()
            add_file(tar, name, data)
        data = (json.dumps(manifest, separators=(",", ":")) + "\n").encode()
        add_file(tar, "runtime-files.json", data)
    print("Manifest SHA-256", hashlib.sha256(data).hexdigest())
    print("Archive SHA-256 ", sha256(archive))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("archive", type=Path)
    output = parser.parse_args().archive.resolve()
    # Construct the entire output beside its destination before publishing it.
    with tempfile.TemporaryDirectory(dir=output.parent) as temporary:
        root = Path(temporary)
        stage = root / "tree"
        stage.mkdir()
        artifact = root / "runtime.tar"
        pack(stage, artifact)
        os.replace(artifact, output)
