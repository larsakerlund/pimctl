#!/bin/sh
#
# Proves install.sh installs a pimctl that runs, and refuses one that arrived
# wrong.
#
# It runs the real thing against the real latest release — an installer that is
# only ever exercised against a stub is an installer nobody has tested.
#
# install.sh needs no credential of its own. This script still resolves one from
# $GITHUB_TOKEN or a logged-in gh, because the repository it downloads from is
# not readable without one yet; once it is, the token stops being necessary and
# PIMCTL_PUBLIC=1 turns on the check that proves it.
#
# Nothing is installed outside the temporary directory this script makes, and
# the expected version is resolved from the GitHub API here rather than taken
# from install.sh's own output, so the two have to agree independently.
#
# Usage:
#   make test-install
#   sh scripts/test-install.sh

set -eu

repo="${PIMCTL_REPO:-larsakerlund/pimctl}"
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
checks=0

fail() {
  printf '\n!!! FAIL: %s\n' "$1" >&2
  exit 1
}

# pass numbers the checks as they go, so a truncated run is obvious from the
# output alone.
pass() {
  printf '\n>>> CHECK %s PASS: %s\n' "$checks" "$1"
  checks=$((checks + 1))
}

work=$(mktemp -d "${TMPDIR:-/tmp}/pimctl-install-test.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

# resolve_token finds a credential to hand the installer, if the machine has one.
#
# It is not an error to have none: PIMCTL_PUBLIC=1 says the repository can be
# read without one, and the checks below then run exactly as a stranger's
# install would.
resolve_token() {
  token="${GITHUB_TOKEN:-}"
  if [ -z "$token" ] && command -v gh >/dev/null 2>&1; then
    token=$(gh auth token --hostname github.com 2>/dev/null || true)
  fi
  if [ -z "$token" ] && [ -z "${PIMCTL_PUBLIC:-}" ]; then
    fail "no token: set GITHUB_TOKEN, log in with gh, or set PIMCTL_PUBLIC=1 once $repo is public"
  fi
}

# resolve_expected_tag asks the API which release install.sh ought to pick, so
# the version check below compares two independent answers.
resolve_expected_tag() {
  if [ -n "${PIMCTL_VERSION:-}" ]; then
    expected_tag="$PIMCTL_VERSION"
    return
  fi
  : >"$work/curl.conf"
  chmod 600 "$work/curl.conf"
  if [ -n "$token" ]; then
    printf 'header = "Authorization: Bearer %s"\n' "$token" >"$work/curl.conf"
  fi
  curl -sSL --config "$work/curl.conf" \
    --header "Accept: application/vnd.github+json" \
    --output "$work/latest.json" \
    "https://api.github.com/repos/$repo/releases/latest" ||
    fail "cannot read the latest release"
  expected_tag=$(tr ',' '\n' <"$work/latest.json" |
    sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' |
    head -n 1)
  [ -n "$expected_tag" ] || fail "the API did not name a latest release"
}

# installs_a_working_binary runs the installer as the one-liner does and checks
# what landed: the binary is where it was asked to go, it is executable, and it
# reports the version of the release the API named.
installs_a_working_binary() {
  printf '=== install.sh installs the latest release ===\n'
  bindir="$work/bin"
  GITHUB_TOKEN="$token" PIMCTL_INSTALL_DIR="$bindir" \
    sh "$root/install.sh" >"$work/install.out" 2>&1 ||
    fail "install.sh exited non-zero: $(cat "$work/install.out")"
  cat "$work/install.out"

  [ -f "$bindir/pimctl" ] || fail "no binary at $bindir/pimctl"
  [ -x "$bindir/pimctl" ] || fail "$bindir/pimctl is not executable"
  pass "the binary was installed into PIMCTL_INSTALL_DIR, executable"

  got=$("$bindir/pimctl" version) || fail "the installed binary does not run"
  [ "$got" = "pimctl ${expected_tag#v}" ] ||
    fail "installed binary reports '$got', expected 'pimctl ${expected_tag#v}'"
  pass "it runs and reports $expected_tag"
}

# refuses_a_tampered_download is the check the checksum exists for. The download
# is corrupted through install.sh's one test hook, and the demand is not just a
# non-zero exit: the install directory must not exist afterwards, because a
# refusal that still left a binary behind would be worse than no check at all.
refuses_a_tampered_download() {
  printf '\n=== install.sh refuses an archive that does not match checksums.txt ===\n'
  baddir="$work/bin-tampered"
  set +e
  GITHUB_TOKEN="$token" PIMCTL_TEST_CORRUPT=1 PIMCTL_INSTALL_DIR="$baddir" \
    sh "$root/install.sh" >"$work/tampered.out" 2>&1
  rc=$?
  set -e
  cat "$work/tampered.out"

  [ "$rc" -ne 0 ] || fail "install.sh accepted a corrupted archive"
  grep -q 'checksum mismatch' "$work/tampered.out" ||
    fail "the refusal does not say the checksum did not match"
  pass "a corrupted archive is refused, and the message says why"

  [ ! -e "$baddir/pimctl" ] || fail "a binary was installed anyway"
  [ ! -d "$baddir" ] || fail "$baddir was created despite the refusal"
  pass "nothing was installed"
}

# prints_its_usage_without_touching_anything proves `-h` answers on a machine
# with no network and no credential: it is the one thing a reader can run to
# find out what the script would do.
prints_its_usage_without_touching_anything() {
  printf '\n=== install.sh -h explains itself ===\n'
  set +e
  env -u GITHUB_TOKEN sh "$root/install.sh" -h >"$work/usage.out" 2>&1
  rc=$?
  set -e
  cat "$work/usage.out"

  [ "$rc" -eq 0 ] || fail "install.sh -h exited $rc"
  grep -q 'PIMCTL_INSTALL_DIR' "$work/usage.out" ||
    fail "the usage does not mention PIMCTL_INSTALL_DIR"
  grep -q 'Not needed to install' "$work/usage.out" ||
    fail "the usage does not say a token is optional"
  pass "-h prints the usage and exits 0"
}

# installs_without_a_token is the check that the one-liner in the README works
# for someone who has never heard of a GitHub token.
#
# It is behind PIMCTL_PUBLIC=1 because it can only pass once the repository is
# public: an unauthenticated read of a repository you cannot see returns 404,
# and a check that fails for a reason unrelated to the code under test teaches
# nobody anything. On flip day, run `PIMCTL_PUBLIC=1 make test-install`, watch
# this pass, and delete the gate.
installs_without_a_token() {
  if [ -z "${PIMCTL_PUBLIC:-}" ]; then
    printf '\n=== skipped: install.sh with no token (set PIMCTL_PUBLIC=1 once %s is public) ===\n' "$repo"
    return
  fi
  printf '\n=== install.sh installs with no token at all ===\n'
  puredir="$work/bin-public"
  env -u GITHUB_TOKEN -u GH_TOKEN PIMCTL_INSTALL_DIR="$puredir" \
    sh "$root/install.sh" >"$work/public.out" 2>&1 ||
    fail "install.sh needs a token: $(cat "$work/public.out")"
  cat "$work/public.out"

  got=$("$puredir/pimctl" version) || fail "the installed binary does not run"
  [ "$got" = "pimctl ${expected_tag#v}" ] ||
    fail "installed binary reports '$got', expected 'pimctl ${expected_tag#v}'"
  pass "an unauthenticated install works and reports $expected_tag"
}

resolve_token
resolve_expected_tag
installs_a_working_binary
refuses_a_tampered_download
prints_its_usage_without_touching_anything
installs_without_a_token

printf '\n=== INSTALL: PASSED (%s checks) ===\n' "$checks"
