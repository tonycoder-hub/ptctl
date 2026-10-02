#!/usr/bin/env python3
"""Package CI-built Windows amd64 binaries, then verify the extracted delivery."""
import argparse
import base64
import hashlib
import json
from pathlib import Path
import re
import struct
import subprocess
import sys
import zipfile


ROOT = Path(__file__).resolve().parents[1]


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def windows_amd64(data):
    if data[:2] != b"MZ" or len(data) < 64:
        return False
    offset = struct.unpack_from("<I", data, 0x3C)[0]
    return (offset + 6 <= len(data) and data[offset:offset + 4] == b"PE\0\0"
            and struct.unpack_from("<H", data, offset + 4)[0] == 0x8664)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--commit", required=True)
    args = parser.parse_args()
    if not re.fullmatch(r"[0-9a-f]{40}", args.commit):
        parser.error("--commit must be the full checked-out Git SHA")
    entries = {name: (ROOT / "dist" / name).read_bytes() for name in ("pt.exe", "ptctl.exe")}
    if not all(windows_amd64(data) for data in entries.values()):
        raise ValueError("both command entry points must be Windows amd64 PE binaries")
    for name in ("LICENSE", "NOTICE", "docs/READ_ONLY_QUICKSTART.md"):
        entries[name] = (ROOT / name).read_bytes()
    sample_root = "examples/readonly/"
    for name in ("README.md", "demo.txt", "demo.torrent.b64", "expected.json"):
        entries[sample_root + name] = (ROOT / sample_root / name).read_bytes()
    entries[sample_root + "demo.torrent"] = base64.b64decode(
        entries[sample_root + "demo.torrent.b64"].strip(), validate=True,
    )
    entries["build.json"] = (json.dumps({
        "product": "pt cli", "version": args.version, "commit": args.commit,
        "os": "windows", "arch": "amd64", "channel": "ci-preview",
    }, indent=2) + "\n").encode()
    entries["SHA256SUMS"] = "".join(
        f"{sha256(data)}  {name}\n" for name, data in sorted(entries.items())
    ).encode()
    output = ROOT / "dist" / "windows"
    output.mkdir(parents=True, exist_ok=True)
    archive = output / f"pt-cli-windows-amd64-{args.commit[:12]}.zip"
    with zipfile.ZipFile(archive, "x", compression=zipfile.ZIP_DEFLATED) as zipped:
        for name, data in sorted(entries.items()):
            zipped.writestr(name, data)
    digest = sha256(archive.read_bytes())
    (output / "SHA256SUMS").write_bytes(f"{digest}  {archive.name}\n".encode())
    # Only this invocation's allowlisted archive is extracted, into a new directory.
    extracted = output / "verified"
    extracted.mkdir()
    with zipfile.ZipFile(archive) as zipped:
        if set(zipped.namelist()) != set(entries) or zipped.testzip() is not None:
            raise ValueError("archive contents or CRC differ")
        zipped.extractall(extracted)
    for name, data in entries.items():
        if sha256((extracted / name).read_bytes()) != sha256(data):
            raise ValueError(f"extracted file checksum differs: {name}")
    # Exercise the delivered binaries and decoded sample, including their identity.
    subprocess.run([
        sys.executable, str(ROOT / "scripts" / "readonly_acceptance.py"),
        "--bin", str(extracted / "pt.exe"), "--legacy-bin", str(extracted / "ptctl.exe"),
        "--samples", str(extracted / "examples" / "readonly"),
        "--version", args.version, "--commit", args.commit,
    ], check=True, timeout=180)
    print(f"PASS package: {archive.name}, SHA-256 {digest}")


if __name__ == "__main__":
    main()
