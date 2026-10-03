#!/usr/bin/env python3
"""Cross-platform acceptance for built binaries; synthetic bytes and loopback mocks only."""
import argparse
import base64
from contextlib import contextmanager
import hashlib
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
from pathlib import Path
import subprocess
import tempfile
import threading
from urllib.parse import parse_qs, urlsplit


SAMPLES = Path(__file__).resolve().parents[1] / "examples" / "readonly"
CANARY = "SYNTHETIC-DOWNLOADER-PASSWORD"
TRACKER_CANARY = "SYNTHETIC-TRACKER-PASSKEY"


def check(condition, message):
    if not condition:
        raise AssertionError(message)


def invoke(binary, args, expected_code=0, stdin="", kind=None):
    result = subprocess.run(
        [str(binary), *map(str, args)], input=stdin, text=True,
        encoding="utf-8", capture_output=True, timeout=30,
    )
    check(result.returncode == expected_code,
          f"{args[:2]}: exit {result.returncode}, expected {expected_code}: {result.stderr}")
    for secret in (CANARY, TRACKER_CANARY):
        check(secret not in result.stdout + result.stderr, "synthetic credential leaked")
    if expected_code == 0:
        check(not result.stderr, f"unexpected stderr: {result.stderr}")
    if kind is None:
        return result.stdout
    envelope = json.loads(result.stdout)
    check(envelope["schema"] == "ptctl.dev/v1", "legacy JSON schema changed")
    check(envelope["kind"] == kind, "unexpected report kind")
    return envelope["data"]


def snapshot(root):
    return {
        p.relative_to(root).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
        if p.is_file() else "directory"
        for p in root.rglob("*")
    }


def json_strings(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for key, item in value.items():
            yield key
            yield from json_strings(item)
    elif isinstance(value, list):
        for item in value:
            yield from json_strings(item)


@contextmanager
def fake_downloader(driver, infohash, length, wrong_path=False):
    requests = []
    client_root = "/elsewhere" if wrong_path else "/downloads"

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def reply(self, body, status=200, headers=None):
            raw = body if isinstance(body, bytes) else json.dumps(body).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            for name, value in (headers or {}).items():
                self.send_header(name, value)
            self.end_headers()
            self.wfile.write(raw)

        def do_GET(self):
            path = urlsplit(self.path).path
            requests.append(("GET", path))
            if (driver == "qbittorrent" and path == "/api/v2/torrents/info"
                    and self.headers.get("Cookie") == "SID=synthetic"):
                self.reply([{
                    "hash": "synthetic-job", "magnet_uri": "magnet:?xt=urn:btih:" + infohash + "&tr=https%3A%2F%2Ftracker.invalid%2F" + TRACKER_CANARY,
                    "name": "demo.txt", "size": length, "progress": 1.0,
                    "state": "uploading", "save_path": client_root,
                    "content_path": client_root + "/demo.txt", "downloaded": length, "uploaded": 0,
                }])
            else:
                self.reply({}, 405)

        def do_POST(self):
            path = urlsplit(self.path).path
            raw = self.rfile.read(int(self.headers.get("Content-Length", "0")))
            if driver == "qbittorrent":
                requests.append(("POST", path))
                if (path == "/api/v2/auth/login"
                        and parse_qs(raw.decode()) == {"username": ["synthetic"], "password": [CANARY]}):
                    self.reply(b"Ok.", headers={"Set-Cookie": "SID=synthetic; Path=/"})
                else:
                    self.reply({}, 405)
                return
            auth = "Basic " + base64.b64encode(("synthetic:" + CANARY).encode()).decode()
            if path != "/transmission/rpc" or self.headers.get("Authorization") != auth:
                requests.append(("unexpected", path))
                self.reply({}, 405)
                return
            request = json.loads(raw)
            method = request.get("method")
            requests.append(("POST", method))
            if not self.headers.get("X-Transmission-Session-Id"):
                self.reply(b"", 409, {"X-Transmission-Session-Id": "synthetic",
                                      "X-Transmission-Rpc-Version": "6.0.0"})
                return
            if method == "session_get":
                result = {"version": "4.1.0", "rpc_version_semver": "6.0.0", "rpc_version": 18}
            elif method == "torrent_get":
                result = {"torrents": [{
                    "hash_string": infohash, "name": "demo.txt", "total_size": length,
                    "percent_complete": 1.0, "status": 6, "download_dir": client_root,
                    "downloaded_ever": length, "uploaded_ever": 0,
                }]}
            else:
                self.reply({}, 405)
                return
            self.reply({"jsonrpc": "2.0", "id": request["id"], "result": result})

    with HTTPServer(("127.0.0.1", 0), Handler) as server:
        thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True)
        thread.start()
        try:
            yield f"http://127.0.0.1:{server.server_port}", requests
        finally:
            server.shutdown()
            thread.join(timeout=5)


def bencode(value):
    """Small deterministic encoder for synthetic acceptance fixtures only."""
    if isinstance(value, int):
        return b"i" + str(value).encode() + b"e"
    if isinstance(value, str):
        value = value.encode()
    if isinstance(value, bytes):
        return str(len(value)).encode() + b":" + value
    if isinstance(value, list):
        return b"l" + b"".join(bencode(item) for item in value) + b"e"
    if isinstance(value, dict):
        keys = sorted(value, key=lambda key: key.encode() if isinstance(key, str) else key)
        return b"d" + b"".join(bencode(key) + bencode(value[key]) for key in keys) + b"e"
    raise TypeError("unsupported synthetic bencode value")


def accept_content_formats(binary, root):
    fixtures = root / "formats"
    fixtures.mkdir()
    content = b"x" * 16384 + b"y"
    pieces = hashlib.sha256(content[:16384]).digest() + hashlib.sha256(content[16384:]).digest()
    merkle_root = hashlib.sha256(pieces).digest()
    v2_info = {"file tree": {"payload.bin": {"": {"length": len(content), "pieces root": merkle_root}}},
               "meta version": 2, "name": "payload.bin", "piece length": 16384}
    v2_doc = {"info": v2_info, "piece layers": {merkle_root: pieces}}
    hybrid_content = b"hybrid proof\n"
    cases = [("v2", v2_doc, content, {"bt-v2"}, {"bt-v2"})]
    for label, good_v1, good_v2 in (("hybrid", True, True), ("hybrid-v1-only", True, False), ("hybrid-v2-only", False, True)):
        v1 = hashlib.sha1(hybrid_content if good_v1 else b"wrong").digest()
        v2 = hashlib.sha256(hybrid_content if good_v2 else b"wrong").digest()
        info = {"file tree": {"hybrid.txt": {"": {"length": len(hybrid_content), "pieces root": v2}}},
                "length": len(hybrid_content), "meta version": 2, "name": "hybrid.txt",
                "piece length": 16384, "pieces": v1}
        passing = ({"bt-v1"} if good_v1 else set()) | ({"bt-v2"} if good_v2 else set())
        cases.append((label, {"info": info}, hybrid_content, {"bt-v1", "bt-v2"}, passing))
    for label, document, data, algorithms, passing in cases:
        document["announce"] = "https://tracker.invalid/announce?passkey=" + TRACKER_CANARY
        torrent = fixtures / (label + ".torrent")
        source = fixtures / (label + ".bin")
        corrupt = fixtures / (label + "-corrupt.bin")
        torrent.write_bytes(bencode(document))
        source.write_bytes(data)
        corrupt.write_bytes(data[:-1] + bytes([data[-1] ^ 1]))
        before = snapshot(fixtures)
        manifest = invoke(binary, ["torrent", "inspect", "--output", "json", torrent], kind="metafile.manifest")
        check(manifest["version"] == ("v2" if label == "v2" else "hybrid"), "metafile version differs")
        check(manifest["tracker_origins"] == ["https://tracker.invalid"], "tracker must expose origin only")
        for field, digest in (("info_hash_v1", hashlib.sha1), ("info_hash_v2", hashlib.sha256)):
            if field in manifest:
                check(manifest[field] == digest(bencode(document["info"])).hexdigest(), "typed infohash differs")
        for path, matches in ((source, passing), (corrupt, set())):
            verified = matches == algorithms
            result = invoke(binary, ["torrent", "verify", "--content", path, "--output", "json", torrent],
                            expected_code=0 if verified else 3, kind="content.verification")
            check(result["verified"] is verified, "both hybrid hash families must agree")
            check({item["algorithm"] for item in result["checks"]} == algorithms, "proof family missing")
            check({item["algorithm"] for item in result["checks"] if item["verified"]} == matches,
                  "individual hash family result differs")
        check(snapshot(fixtures) == before, "format verification wrote fixture data")
    # A piece crosses a file boundary; a zero-length physical file participates.
    torrent = fixtures / "multi.torrent"
    bundle = fixtures / "bundle"
    bundle.mkdir()
    for name, data in (("a.bin", b"abc"), ("b.bin", b"def"), ("empty.bin", b"")):
        (bundle / name).write_bytes(data)
    info = {"name": "bundle", "piece length": 4,
            "files": [{"length": size, "path": [name]} for name, size in (("a.bin", 3), ("b.bin", 3), ("empty.bin", 0))],
            "pieces": hashlib.sha1(b"abcd").digest() + hashlib.sha1(b"ef").digest()}
    torrent.write_bytes(bencode({"info": info}))
    corrupt_bundle = fixtures / "bundle-corrupt"
    corrupt_bundle.mkdir()
    for name, data in (("a.bin", b"abc"), ("b.bin", b"deg"), ("empty.bin", b"")):
        (corrupt_bundle / name).write_bytes(data)
    before = snapshot(fixtures)
    manifest = invoke(binary, ["torrent", "inspect", "--output", "json", torrent], kind="metafile.manifest")
    check(manifest["multi_file"] and manifest["total_length"] == 6, "multi-file layout differs")
    result = invoke(binary, ["torrent", "verify", "--content", bundle, "--output", "json", torrent], kind="content.verification")
    check(result["verified"] and result["pieces_matched"] == 2 and result["files_checked"] == 3,
          "cross-file piece or empty-file proof missing")
    result = invoke(binary, ["torrent", "verify", "--content", corrupt_bundle, "--output", "json", torrent],
                    expected_code=3, kind="content.verification")
    check(not result["verified"] and result["pieces_matched"] == 1 and result["mismatch_pieces"] == [1],
          "cross-file corruption must identify the failing piece")
    check(snapshot(fixtures) == before, "multi-file verification wrote fixture data")


def accept(binary, version, commit, samples):
    identity = invoke(binary, ["version", "--output", "json"], kind="version")
    check(identity == {"product": "pt cli", "version": version, "commit": commit},
          "build identity differs from the checked-out commit")
    check(invoke(binary, ["--version"]) == f"pt cli {version} ({commit})\n", "version alias differs")
    help_text = invoke(binary, ["help"])
    check(help_text.startswith("pt cli") and "ptctl" in help_text, "brand or compatibility help missing")
    for group in ("torrent", "client", "reconcile"):
        direct = invoke(binary, [group, "--help"])
        check(direct.startswith("Usage:") and direct == invoke(binary, ["help", group]),
              "group help and topic help must agree")
    check("no automatic discovery" in invoke(binary, ["help", "config"]), "configuration model is undiscoverable")
    check("-content" in invoke(binary, ["help", "torrent", "verify"]), "leaf help topic is missing")
    invoke(binary, ["help", "client", "remove", "run", "--password-stdin"], expected_code=2)
    for prefix in (("version",), ("torrent", "inspect"), ("torrent", "verify"), ("client", "list"), ("client", "status"), ("reconcile", "report")):
        text = invoke(binary, [*prefix, "--help"])
        check(text.startswith("Usage:") and "Flags:" in text, "leaf help is missing")
    invoke(binary, ["torrent", "inspect", "--unknown-read-only-flag"], expected_code=2)
    expected = json.loads((samples / "expected.json").read_text(encoding="utf-8"))
    raw = base64.b64decode((samples / "demo.torrent.b64").read_text().strip(), validate=True)
    if (samples / "demo.torrent").exists():
        check((samples / "demo.torrent").read_bytes() == raw, "packaged metafile differs")
    content = (samples / "demo.txt").read_bytes()
    check(hashlib.sha256(raw).hexdigest() == expected["metafile_sha256"], "sample metafile changed")
    check(hashlib.sha256(content).hexdigest() == expected["content_sha256"], "sample content changed")
    with tempfile.TemporaryDirectory(prefix="pt-cli-readonly-") as name:
        root = Path(name).resolve()
        content_root = root / "content"
        content_root.mkdir()
        source = content_root / "demo.txt"
        source.write_bytes(content)
        torrent = root / "demo.torrent"
        torrent.write_bytes(raw)
        corrupt = root / "same-size-corrupt.txt"
        corrupt.write_bytes(bytes([content[0] ^ 1]) + content[1:])
        accept_content_formats(binary, root)
        invoke(binary, ["torrent", "inspect", root / "missing.torrent"], expected_code=1)
        before = snapshot(root)
        manifest = invoke(binary, ["torrent", "inspect", "--output", "json", torrent], kind="metafile.manifest")
        check(manifest["info_hash_v1"] == expected["info_hash_v1"], "infohash differs")
        check(manifest["total_length"] == len(content) and manifest["private"], "sample metadata differs")
        for path, code, verified in [(source, 0, True), (corrupt, 3, False)]:
            result = invoke(binary, ["torrent", "verify", "--content", path, "--output", "json", torrent],
                            expected_code=code, kind="content.verification")
            check(result["verified"] is verified, "verification result differs")
            check(result["pieces_expected"] == 1 and result["pieces_matched"] == int(verified),
                  "exact piece proof differs")
        local = ["reconcile", "report", "--torrent", torrent, "--source", source, "--output", "json"]
        for suffix, code in [([], 0), (["--require-reconciled"], 4)]:
            report = invoke(binary, local + suffix, expected_code=code, kind="ledger.reconciliation")
            check(report["outcome"] == "partial" and report["writes_performed"] == 0,
                  "local-only reconciliation must remain partial and read-only")
            check(report["ledgers"]["storage"]["status"] == "verified_exact_root", "local proof missing")
        for driver in ("qbittorrent", "transmission"):
            for wrong_path in (False, True):
                with fake_downloader(driver, expected["info_hash_v1"], len(content), wrong_path) as (url, requests):
                    report = invoke(binary, [
                        "reconcile", "report", "--torrent", torrent, "--search-root", content_root,
                        "--driver", driver, "--url", url, "--username", "synthetic", "--password-stdin",
                        "--host-root", content_root, "--client-root", "/downloads", "--client-style", "posix",
                        "--require-reconciled", "--timeout", "10s", "--output", "json",
                    ], expected_code=4 if wrong_path else 0, stdin=CANARY + "\n", kind="ledger.reconciliation")
                    expected_requests = (
                        [("POST", "/api/v2/auth/login"), ("GET", "/api/v2/torrents/info"), ("GET", "/api/v2/torrents/info")]
                        if driver == "qbittorrent" else
                        [("POST", "session-get"), ("POST", "session_get"), ("POST", "torrent_get"), ("POST", "torrent_get")]
                    )
                    check(requests == expected_requests, f"unexpected {driver} requests: {requests}")
                    check(report["outcome"] == ("partial" if wrong_path else "consistent") and report["writes_performed"] == 0,
                          f"{driver} reconciliation must respect path conflicts and remain read-only")
                    if wrong_path:
                        check(any(item["code"] == "path.verified_source_differs_from_job" for item in report["blockers"]),
                              "path disagreement must retain its explicit blocker")
                    check(report["ledgers"]["downloader"]["requests_made"] == len(requests), "request accounting differs")
                    relations = {item["kind"]: item["status"] for item in report["relations"]}
                    check(relations["client_infohash_relation"] == "exact_unique", "client identity proof missing")
                    check(relations["verified_source_vs_job_path"] == ("different_location" if wrong_path else "same_location"),
                          "path mapping disagreement was lost")
                    check(relations["metafile_variant_relation"] == "unobservable", "private variant proof was overstated")
                    strings = list(json_strings(report))
                    for private in (str(root), url, "/downloads/demo.txt", "/elsewhere/demo.txt", CANARY, "magnet:?"):
                        check(all(private not in value for value in strings),
                              "default report disclosed private context")
        check(snapshot(root) == before, "read-only commands changed sample files or directory entries")
    print(f"PASS {binary.name}: identity/help, v1 multi-file/v2/hybrid, exit 0/1/2/3/4, qBittorrent/Transmission paths, privacy, zero writes")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bin", type=Path, required=True)
    parser.add_argument("--legacy-bin", type=Path, required=True)
    parser.add_argument("--samples", type=Path, default=SAMPLES)
    parser.add_argument("--version", required=True)
    parser.add_argument("--commit", required=True)
    args = parser.parse_args()
    for binary in (args.bin, args.legacy_bin):
        accept(binary.resolve(strict=True), args.version, args.commit, args.samples.resolve(strict=True))


if __name__ == "__main__":
    main()
