#!/usr/bin/env python3
"""Deterministic native packages; compare two builds and accept the extracted delivery."""
import argparse
import base64
import hashlib
import io
import json
import os
from pathlib import Path
import re
import stat
import struct
import subprocess
import sys
import zipfile

ROOT = Path(__file__).resolve().parents[1]
TARGETS = (("windows", "amd64"), ("linux", "amd64"), ("darwin", "arm64"))
DOCUMENTS = ("LICENSE", "NOTICE", "docs/READ_ONLY_QUICKSTART.md",
             "examples/readonly/README.md", "examples/readonly/demo.txt",
             "examples/readonly/demo.torrent.b64", "examples/readonly/expected.json")


def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def binary_names(goos):
    suffix = ".exe" if goos == "windows" else ""
    return ("pt" + suffix, "ptctl" + suffix)


def architecture(data, goos):
    if goos == "windows":
        if len(data) < 64 or data[:2] != b"MZ":
            return False
        offset = struct.unpack_from("<I", data, 0x3C)[0]
        return (offset + 6 <= len(data) and data[offset:offset + 4] == b"PE\0\0"
                and struct.unpack_from("<H", data, offset + 4)[0] == 0x8664)
    if goos == "linux":
        return (len(data) >= 20 and data[:6] == b"\x7fELF\x02\x01"
                and struct.unpack_from("<H", data, 18)[0] == 62)
    return (len(data) >= 8 and data[:4] == b"\xcf\xfa\xed\xfe"
            and struct.unpack_from("<I", data, 4)[0] == 0x0100000C)


def canonical_zip(entries, executables):
    result = io.BytesIO()
    with zipfile.ZipFile(result, "w", compression=zipfile.ZIP_STORED) as archive:
        for name, data in sorted(entries.items()):
            entry = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
            entry.create_system = 3
            entry.external_attr = (stat.S_IFREG | (0o755 if name in executables else 0o644)) << 16
            archive.writestr(entry, data)
    return result.getvalue()


def make_package(binaries, version, commit, goos, goarch, channel, go_version):
    require((goos, goarch) in TARGETS, "unsupported native target")
    require(re.fullmatch(r"[0-9a-f]{40}", commit), "full commit required")
    require(re.fullmatch(r"v[0-9A-Za-z.+-]+", version), "unsafe version")
    names = binary_names(goos)
    entries = {name: (binaries / name).read_bytes() for name in names}
    require(all(architecture(data, goos) for data in entries.values()), "binary architecture mismatch")
    entries.update({name: (ROOT / name).read_bytes() for name in DOCUMENTS})
    entries["examples/readonly/demo.torrent"] = base64.b64decode(
        entries["examples/readonly/demo.torrent.b64"].strip(), validate=True)
    entries["build.json"] = (json.dumps({
        "product": "pt cli", "version": version, "commit": commit,
        "os": goos, "arch": goarch, "channel": channel, "go_version": go_version,
    }, sort_keys=True, indent=2) + "\n").encode()
    entries["SHA256SUMS"] = "".join(
        f"{sha256(data)}  {name}\n" for name, data in sorted(entries.items())).encode()
    return canonical_zip(entries, names)


def inspect_package(data, version, commit, goos, goarch, channel):
    names = binary_names(goos)
    expected = set(DOCUMENTS) | set(names) | {"build.json", "SHA256SUMS", "examples/readonly/demo.torrent"}
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        require(len(archive.namelist()) == len(expected) and set(archive.namelist()) == expected,
                "package members differ")
        require(archive.testzip() is None, "package CRC mismatch")
        sums = [line.split("  ", 1) for line in archive.read("SHA256SUMS").decode().splitlines()]
        require(len(sums) == len(expected) - 1 and {name for _, name in sums} == expected - {"SHA256SUMS"},
                "package checksum manifest differs")
        for digest, name in sums:
            require(sha256(archive.read(name)) == digest, "package checksum mismatch: " + name)
        build = json.loads(archive.read("build.json"))
        for key, value in {"product": "pt cli", "version": version, "commit": commit,
                           "os": goos, "arch": goarch, "channel": channel}.items():
            require(build.get(key) == value, "package identity mismatch: " + key)
        for name in names:
            require(architecture(archive.read(name), goos), "package binary architecture differs")
            require((archive.getinfo(name).external_attr >> 16) & 0o777 == 0o755,
                    "executable mode missing")
        return build


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("binaries", "compare-binaries", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    for name in ("version", "commit", "goos", "goarch", "go-version"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--channel", choices=("ci-preview", "release"), required=True)
    args = parser.parse_args()
    options = (args.version, args.commit, args.goos, args.goarch, args.channel, args.go_version)
    data = make_package(args.binaries, *options)
    require(data == make_package(args.compare_binaries, *options), "two independent build packages differ")
    inspect_package(data, *options[:5])
    args.output.mkdir(parents=True, exist_ok=False)
    archive = args.output / f"pt-cli-{args.version}-{args.goos}-{args.goarch}.zip"
    archive.write_bytes(data)
    (args.output / "SHA256SUMS").write_bytes(f"{sha256(data)}  {archive.name}\n".encode())
    extracted = args.output / "verified"
    # Use the consumer's native extractor, including Unix executable-mode behavior.
    if args.goos == "windows":
        subprocess.run(["pwsh", "-NoProfile", "-NonInteractive", "-Command",
                        "Expand-Archive -LiteralPath $env:PT_ARCHIVE -DestinationPath $env:PT_EXTRACTED"],
                       env={**os.environ, "PT_ARCHIVE": str(archive.resolve()),
                            "PT_EXTRACTED": str(extracted.resolve())}, check=True, timeout=120)
    else:
        subprocess.run(["unzip", "-q", str(archive.resolve()), "-d", str(extracted.resolve())],
                       check=True, timeout=120)
    with zipfile.ZipFile(archive) as packed:
        for name in packed.namelist():
            require((extracted / name).read_bytes() == packed.read(name), "extracted bytes differ: " + name)
    names = binary_names(args.goos)
    subprocess.run([sys.executable, str(ROOT / "scripts/readonly_acceptance.py"),
                    "--bin", str(extracted / names[0]), "--legacy-bin", str(extracted / names[1]),
                    "--samples", str(extracted / "examples/readonly"),
                    "--version", args.version, "--commit", args.commit], check=True, timeout=180)
    print(f"PASS reproducible native package: {archive.name}, SHA-256 {sha256(data)}")


if __name__ == "__main__":
    main()
