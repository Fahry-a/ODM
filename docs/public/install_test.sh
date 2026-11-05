#!/bin/sh
# Hermetic test for install.sh's mandatory checksum verification.
# Extracts the real "verify checksum" block out of install.sh and runs it
# against fixtures, with a stubbed curl — no network involved.
#
# Exit codes: 0 = all cases pass, 1 = a case failed, 77 = sha256sum missing
# (skip, autotools convention).
set -u

if ! command -v sha256sum >/dev/null 2>&1; then
	echo "SKIP: sha256sum not available" >&2
	exit 77
fi

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SRC="$here/install.sh"

# The pieces of install.sh under test, verbatim — a test that re-implements
# the logic would pass on a stale copy.
TOP=$(sed -n '1,/^# --- detect platform/p' "$SRC" | sed '$d')
VERIFY=$(sed -n '/^# verify checksum/,/^# --- extract/p' "$SRC" | sed '$d')

if [ -z "$VERIFY" ]; then
	echo "FAIL: could not extract the verify-checksum block from $SRC" >&2
	exit 1
fi

failures=0

# run_case <name> <want-ok|want-fail> <fixture-mode>
#   fixture-mode: good | fetch-fail | missing-entry | wrong-hash
run_case() {
	name=$1
	want=$2
	mode=$3

	TMPD=$(mktemp -d)
	STUB="$TMPD/bin"
	mkdir -p "$STUB"

	# Fixture release payload + its checksums.txt (as release.yml generates).
	printf 'odm-fake-payload\n' >"$TMPD/tarball-src"
	TARBALL_NAME="odm_1.9.2_linux_amd64.tar.gz"
	hash=$(sha256sum "$TMPD/tarball-src" | awk '{print $1}')
	case "$mode" in
	good)
		printf '%s  %s\n' "$hash" "$TARBALL_NAME" >"$TMPD/sums.txt" ;;
	missing-entry)
		printf '%s  %s\n' "$hash" "odm_1.9.2_linux_arm64.tar.gz" >"$TMPD/sums.txt" ;;
	wrong-hash)
		printf '%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" "$TARBALL_NAME" >"$TMPD/sums.txt" ;;
	fetch-fail)
		: >/dev/null ;; # no sums.txt fixture; curl stub exits 22
	esac

	# curl stub: understands `-o <file> <url>` regardless of option order.
	cat >"$STUB/curl" <<EOF
#!/bin/sh
out=; prev=
for a in "\$@"; do
  if [ "\$prev" = "-o" ]; then out="\$a"; fi
  prev="\$a"
done
if [ "$mode" != "fetch-fail" ] && [ -n "\$out" ]; then
  cp "$TMPD/sums.txt" "\$out"
  exit 0
fi
exit 22
EOF
	chmod +x "$STUB/curl"

	out=$(PATH="$STUB:$PATH" TOP="$TOP" VERIFY="$VERIFY" TMPD="$TMPD" TARBALL_NAME="$TARBALL_NAME" sh -c '
		eval "$TOP"
		TMPDIR="$TMPD"
		TARBALL="$TMPDIR/$TARBALL_NAME"
		cp "$TMPDIR/tarball-src" "$TARBALL"
		CHECKSUM_URL="https://example.invalid/sums.txt"
		eval "$VERIFY"
		echo VERIFY_OK
	' 2>&1)
	rc=$?

	if [ "$want" = "want-ok" ]; then
		if [ $rc -eq 0 ] && printf '%s' "$out" | grep -q VERIFY_OK; then
			echo "PASS: $name"
		else
			echo "FAIL: $name (rc=$rc)" >&2; printf '%s\n' "$out" >&2; failures=$((failures + 1))
		fi
	else
		if [ $rc -ne 0 ]; then
			echo "PASS: $name"
		else
			echo "FAIL: $name (rc=0, expected refusal to install)" >&2; printf '%s\n' "$out" >&2; failures=$((failures + 1))
		fi
	fi

	rm -rf "$TMPD"
}

run_case "valid checksum passes"          want-ok   good
run_case "checksums.txt fetch failure"    want-fail fetch-fail
run_case "tarball missing from sums"      want-fail missing-entry
run_case "wrong hash is refused"          want-fail wrong-hash

if [ "$failures" -ne 0 ]; then
	echo "$failures case(s) failed" >&2
	exit 1
fi
echo "install.sh checksum verification: all cases pass"
