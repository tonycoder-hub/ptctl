#!/bin/sh
# An offline user walkthrough for an extracted macOS/Linux package.
set -eu
if [ "$#" -ne 0 ]; then
    printf 'Usage: sh examples/readonly/try.sh\nNo endpoints, credentials, or user files are used.\n'
    case "$*" in -h|--help) exit 0 ;; *) exit 2 ;; esac
fi
sample_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
package_dir=$(CDPATH= cd -- "$sample_dir/../.." && pwd -P)
cd "$package_dir"
if [ ! -x ./pt ]; then
    printf 'Run this script from an extracted macOS/Linux package containing pt.\n' >&2
    exit 1
fi

printf '\n[1/5] Build identity\n'
./pt version
printf '\n[2/5] Inspect the bundled synthetic torrent\n'
./pt torrent inspect examples/readonly/demo.torrent
printf '\n[3/5] Verify its exact content (expect verified=true)\n'
./pt torrent verify --content examples/readonly/demo.txt examples/readonly/demo.torrent
demo_dir=$(mktemp -d "${TMPDIR:-/tmp}/pt-cli-demo.XXXXXX")
trap 'rm -f "$demo_dir/report.txt" "$demo_dir/corrupt.bin"; rmdir "$demo_dir"' 0
printf '\n[4/5] Read-only local reconciliation (expect partial, zero writes, exit 4)\n'
if ./pt reconcile report --torrent examples/readonly/demo.torrent --source examples/readonly/demo.txt --require-reconciled > "$demo_dir/report.txt"; then
    printf 'Unexpected success: no downloader ledger was supplied.\n' >&2
    exit 1
else
    code=$?
    [ "$code" -eq 4 ] || exit "$code"
fi
sed -n '1,9p' "$demo_dir/report.txt"
printf 'Full report: ./pt reconcile report --torrent examples/readonly/demo.torrent --source examples/readonly/demo.txt\n'
printf '\n[5/5] Detect a changed byte in a temporary sample copy (expect exit 3)\n'
corrupt_file="$demo_dir/corrupt.bin"
cp examples/readonly/demo.txt "$corrupt_file"
printf X | dd of="$corrupt_file" bs=1 count=1 conv=notrunc 2>/dev/null
if ./pt torrent verify --content "$corrupt_file" examples/readonly/demo.torrent; then
    printf 'Unexpected success: changed content must fail verification.\n' >&2
    exit 1
else
    code=$?
    [ "$code" -eq 3 ] || exit "$code"
fi
printf '\nPASS: all five offline checks behaved as expected.\n'
printf 'The original samples are unchanged. No downloader was contacted.\n'
