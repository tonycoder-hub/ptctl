#!/usr/bin/env python3
"""Decide main-line releases, assemble checked assets, and tag and publish them."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
from urllib.parse import quote

from package import TARGETS, inspect_package, require, sha256


def release_version(value):
    require(re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?", value),
            "release version must be vMAJOR.MINOR.PATCH with an optional prerelease")
    for part in value.partition("-")[2].split("."):
        require(not (part.isdigit() and len(part) > 1 and part.startswith("0")), "numeric prerelease has a leading zero")
    return value


def git(*args, cwd=None):
    return subprocess.check_output(["git", *args], cwd=cwd, text=True, stderr=subprocess.PIPE).strip()


def version_file(content):
    value = content.rstrip("\n")
    require(content == value + "\n", "VERSION must contain one LF-terminated version")
    return release_version(value)


def version_key(value):
    """Order release versions by SemVer precedence."""
    release_version(value)
    core, _, prerelease = value[1:].partition("-")
    numbers = tuple(int(part) for part in core.split("."))
    if not prerelease:
        return numbers, (1,)
    # Numeric identifiers sort before alphanumeric ones; a shorter list sorts first.
    return numbers, (0, tuple((0, int(part), "") if part.isdigit() else (1, 0, part) for part in prerelease.split(".")))


def release_notes(changelog, version):
    """Return the body of the one CHANGELOG.md section headed by version."""
    lines = changelog.split("\n")
    starts = [i for i, line in enumerate(lines) if re.fullmatch(r"## " + re.escape(version) + r"( .*)?", line)]
    require(len(starts) == 1, "CHANGELOG.md needs exactly one section for " + version)
    end = next((i for i in range(starts[0] + 1, len(lines)) if lines[i].startswith("## ")), len(lines))
    body = "\n".join(lines[starts[0] + 1:end]).strip()
    require(body, "CHANGELOG.md section for " + version + " is empty")
    return body + "\n"


def main_line(commit, cwd=None):
    """Require a full commit on main whose workflows are main's own."""
    require(re.fullmatch(r"[0-9a-f]{40}", commit), "full source commit required")
    # Only main-line releases; no branch fallback.
    subprocess.run(["git", "merge-base", "--is-ancestor", commit, "refs/remotes/origin/main"], cwd=cwd, check=True)
    subprocess.run(["git", "diff", "--quiet", "--no-ext-diff", commit, "refs/remotes/origin/main", "--", ".github/workflows"], cwd=cwd, check=True, capture_output=True)


def plan(commit, cwd=None):
    """Decide whether a main-line commit releases its VERSION.

    A VERSION tagged on an earlier commit is not released again. An untagged
    one must be newer than every release tag, and either needs release notes.
    """
    main_line(commit, cwd)
    version = version_file(subprocess.check_output(["git", "show", f"{commit}:VERSION"], cwd=cwd).decode())
    tags = set()
    for name in git("tag", "--list", "v*", cwd=cwd).splitlines():
        try:
            tags.add(release_version(name))
        except ValueError:
            continue
    result = {"commit": commit, "version": version, "channel": "release", "release": "false"}
    if version in tags:
        # A tag on this very commit is a rerun that has to finish the release.
        if git("rev-parse", "--verify", f"refs/tags/{version}^{{commit}}", cwd=cwd) != commit:
            return result
    else:
        require(all(version_key(tag) < version_key(version) for tag in tags), "VERSION must be newer than every release tag")
    release_notes(subprocess.check_output(["git", "show", f"{commit}:CHANGELOG.md"], cwd=cwd).decode(), version)
    result["release"] = "true"
    return result


def identity(version, commit, channel):
    require(re.fullmatch(r"[0-9a-f]{40}", commit), "full source commit required")
    require(channel in ("ci-preview", "release"), "invalid channel")
    release_version(version if channel == "release" else version.partition("+")[0])
    if channel == "ci-preview":
        require(version == version.partition("+")[0] + "+g" + commit[:12], "preview version and commit differ")
    return {"product": "pt cli", "version": version, "commit": commit, "channel": channel}


def assemble(source, output, version, commit, channel):
    manifest = identity(version, commit, channel)
    files = {}
    expected = {f"pt-cli-{version}-{goos}-{arch}.zip": (goos, arch) for goos, arch in TARGETS}
    archives = list(source.rglob("*.zip"))
    require(len(archives) == len(expected) and {p.name for p in archives} == set(expected), "missing, duplicate, or unexpected platform package")
    for archive in archives:
        data = archive.read_bytes()
        require((archive.parent / "SHA256SUMS").read_bytes() == f"{sha256(data)}  {archive.name}\n".encode(), "platform checksum differs")
        inspect_package(data, version, commit, *expected[archive.name], channel)
        files[archive.name] = data
    manifest["packages"] = {name: sha256(data) for name, data in sorted(files.items())}
    files["release-manifest.json"] = (json.dumps(manifest, sort_keys=True, indent=2) + "\n").encode()
    files["SHA256SUMS"] = "".join(f"{sha256(data)}  {name}\n" for name, data in sorted(files.items())).encode()
    output.mkdir(parents=True, exist_ok=False)
    for name, data in files.items():
        (output / name).write_bytes(data)
    return manifest


def verify_assets(directory, tag, commit):
    expected_identity = identity(tag, commit, "release")
    names = {f"pt-cli-{tag}-{goos}-{arch}.zip": (goos, arch) for goos, arch in TARGETS}
    expected = set(names) | {"release-manifest.json", "SHA256SUMS"}
    require({p.name for p in directory.iterdir()} == expected, "release assets are incomplete or unexpected")
    manifest = json.loads((directory / "release-manifest.json").read_text())
    for key, value in expected_identity.items():
        require(manifest.get(key) == value, "release manifest identity differs")
    packages = {}
    for name, target in names.items():
        data = (directory / name).read_bytes()
        inspect_package(data, tag, commit, *target, "release")
        packages[name] = sha256(data)
    require(manifest.get("packages") == packages, "release package digests differ")
    checksums = "".join(f"{sha256((directory / name).read_bytes())}  {name}\n" for name in sorted(expected - {"SHA256SUMS"}))
    require((directory / "SHA256SUMS").read_bytes() == checksums.encode(), "release SHA256SUMS differs")
    return [directory / name for name in sorted(expected)]


def gh_json(endpoint, body=None, upload=None, method="POST"):
    command = ["gh", "api", endpoint, "-H", "X-GitHub-Api-Version: 2022-11-28"]
    payload = None
    if body is not None:
        command += ["--method", method, "--input", "-"]
        payload = json.dumps(body)
    if upload is not None:
        command += ["--method", "POST", "-H", "Content-Type: application/octet-stream", "--input", str(upload)]
    result = subprocess.run(command, input=payload, text=True, capture_output=True, timeout=180)
    require(result.returncode == 0, "GitHub API request failed: " + endpoint)
    return json.loads(result.stdout)


def existing_release(repository, tag):
    # The by-tag endpoint documents published releases only. The authenticated
    # list includes drafts for a writer, so enumerate it without treating 404
    # or any other API failure as permission to create a duplicate.
    for page in range(1, 101):
        releases = gh_json(f"repos/{repository}/releases?per_page=100&page={page}")
        require(isinstance(releases, list), "release listing response differs")
        for item in releases:
            if item.get("tag_name") == tag:
                return item
        if len(releases) < 100:
            return None
    raise ValueError("release enumeration exceeds limit")


def remote_tag(repository, tag):
    obj = gh_json(f"repos/{repository}/git/ref/tags/{quote(tag, safe='')}")["object"]
    for _ in range(10):
        if obj["type"] == "commit":
            return obj["sha"]
        require(obj["type"] == "tag" and re.fullmatch(r"[0-9a-f]{40}", obj["sha"]), "unexpected tag object")
        obj = gh_json(f"repos/{repository}/git/tags/{obj['sha']}")["object"]
    raise ValueError("tag chain exceeds limit")


def find_remote_tag(repository, tag):
    """Return the commit a remote tag names, or None when no such tag exists."""
    # matching-refs answers an absent tag with an empty list, so no failure is
    # ever read as absence.
    refs = gh_json(f"repos/{repository}/git/matching-refs/tags/{quote(tag, safe='')}")
    require(isinstance(refs, list), "tag listing response differs")
    if not any(item.get("ref") == f"refs/tags/{tag}" for item in refs):
        return None
    return remote_tag(repository, tag)


def create_tag(repository, tag, commit):
    """Create one annotated tag for commit; never moves an existing tag."""
    obj = gh_json(f"repos/{repository}/git/tags", body={
        "tag": tag, "message": "pt cli " + tag, "object": commit, "type": "commit"})
    require(re.fullmatch(r"[0-9a-f]{40}", str(obj.get("sha"))) and obj.get("tag") == tag
            and obj.get("object", {}).get("sha") == commit, "tag object response differs")
    ref = gh_json(f"repos/{repository}/git/refs", body={"ref": f"refs/tags/{tag}", "sha": obj["sha"]})
    require(ref.get("ref") == f"refs/tags/{tag}" and ref.get("object", {}).get("sha") == obj["sha"], "tag ref response differs")


def publish(repository, tag, commit, assets, notes, cwd=None):
    """Tag a tested main-line commit and publish its release.

    The release is drafted, filled, and checked before it is made public, so a
    failure at any step leaves at most an unpublished draft.
    """
    release_version(tag)
    require(re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository), "invalid repository")
    main_line(commit, cwd)
    files = verify_assets(assets, tag, commit)  # All local validation precedes any remote write.
    prerelease = "-" in tag
    endpoint = f"repos/{repository}/releases"
    require(existing_release(repository, tag) is None,
            "a release already exists; refusing to replace a draft or published release")
    current = find_remote_tag(repository, tag)
    if current is None:
        create_tag(repository, tag, commit)
    else:
        # A rerun after a failure past tagging finds its own tag.
        require(current == commit, "tag already exists on another commit")
    require(remote_tag(repository, tag) == commit, "remote tag is missing or moved")
    release = gh_json(endpoint, body={
        "tag_name": tag, "target_commitish": commit, "name": "pt cli " + tag,
        "draft": True, "prerelease": prerelease, "make_latest": "false",
        "generate_release_notes": True,
        "body": notes.rstrip("\n") + "\n\n---\n\n"
                f"Native tested packages for `{commit}`. Verify SHA256SUMS before use. "
                "No code signing or notarization is provided.\n",
    })
    require(release.get("draft") is True and isinstance(release.get("id"), int), "draft creation response differs")
    release_id = release["id"]
    # No overwrite, automatic retry, or deletion exists in this helper. Any
    # partial-upload failure leaves an unpublished draft for manual inspection.
    expected = {}
    for path in files:
        expected[path.name] = "sha256:" + sha256(path.read_bytes())
        asset = gh_json(f"https://uploads.github.com/repos/{repository}/releases/{release_id}/assets?name={quote(path.name, safe='')}", upload=path)
        require(asset.get("name") == path.name and asset.get("state") == "uploaded"
                and asset.get("digest") == expected[path.name], "uploaded asset digest/state differs")
    final = gh_json(f"{endpoint}/{release_id}")
    require(final.get("draft") is True and final.get("tag_name") == tag, "release must remain a draft")
    remote_assets = final.get("assets", [])
    require(len(remote_assets) == len(expected) and {a["name"]: a.get("digest") for a in remote_assets} == expected
            and all(a.get("state") == "uploaded" for a in remote_assets), "draft asset set differs")
    require(remote_tag(repository, tag) == commit, "remote tag moved during upload; inspect the unpublished draft")
    public = gh_json(f"{endpoint}/{release_id}", method="PATCH", body={
        "draft": False, "make_latest": "false" if prerelease else "true"})
    require(public.get("draft") is False and public.get("tag_name") == tag and public.get("prerelease") is prerelease
            and {a["name"]: a.get("digest") for a in public.get("assets", [])} == expected, "published release differs")
    return {"draft": False, "url": public["html_url"], "commit": commit, "version": tag}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    meta = sub.add_parser("identity")
    meta.add_argument("--release-version", default="")
    decide = sub.add_parser("plan")
    decide.add_argument("--commit", required=True)
    collect = sub.add_parser("assemble")
    for name in ("source", "output"):
        collect.add_argument("--" + name, type=Path, required=True)
    for name in ("version", "commit", "channel"):
        collect.add_argument("--" + name, required=True)
    release = sub.add_parser("publish")
    for name in ("repository", "tag", "commit"):
        release.add_argument("--" + name, required=True)
    for name in ("assets", "changelog"):
        release.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    if args.command == "identity":
        version = version_file(Path("VERSION").read_bytes().decode())
        # Every VERSION, released or not yet, carries its release notes.
        release_notes(Path("CHANGELOG.md").read_bytes().decode(), version)
        commit = git("rev-parse", "HEAD")
        require(not args.release_version or args.release_version == version, "release version differs from source VERSION")
        result = identity(version if args.release_version else version + "+g" + commit[:12], commit,
                          "release" if args.release_version else "ci-preview")
    elif args.command == "plan":
        result = plan(args.commit)
    elif args.command == "assemble":
        result = assemble(args.source, args.output, args.version, args.commit, args.channel)
    else:
        notes = release_notes(args.changelog.read_bytes().decode(), args.tag)
        result = publish(args.repository, args.tag, args.commit, args.assets, notes)
    if args.command in ("identity", "plan") and os.environ.get("GITHUB_OUTPUT"):
        with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
            for key in ("version", "commit", "channel", "release"):
                if key in result:
                    output.write(f"{key}={result[key]}\n")
    print(json.dumps(result, sort_keys=True))


if __name__ == "__main__":
    main()
