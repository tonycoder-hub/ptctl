# Security policy

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting for this repository. Do not
open a public issue containing a tracker cookie, passkey, private `.torrent`,
RSS URL, downloader credential, private path, or unredacted diagnostic output.

If private reporting is unavailable, open a minimal public issue asking the
maintainer to enable a private channel. Do not include exploit details or
secrets in that issue.

## Supported versions

Until the first stable release, only the latest commit on the default branch is
supported.

## Scope

Security-relevant reports include credential exposure, cross-origin secret
forwarding, SSRF, path traversal, symlink/junction escape, incorrect content
verification, unbounded parsing, and behavior that could damage tracker
accounts or local data.

