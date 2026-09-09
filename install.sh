#!/bin/sh
#
# The one-line installer. It downloads the pimctl release built for this
# machine, checks it against the release's own sha256 list, and leaves the
# binary in ~/.local/bin:
#
#     curl -fsSL https://raw.githubusercontent.com/larsakerlund/pimctl/main/install.sh | sh
#
# It needs no credential, and does not use the GitHub API to avoid needing one:
# the tag comes from where /releases/latest redirects, and the files from the
# release's public download URLs. $GITHUB_TOKEN, when the environment has one,
# switches to the API instead — which is how a repository that requires a
# credential serves its assets — and the one thing the script never does is
# print it.
#
# POSIX sh on purpose: this runs under whatever /bin/sh the machine has, so no
# bashisms, no arrays and no `local`. Functions therefore share one set of
# variables, named for what they hold. `make lint` runs shellcheck over it.
#
# Everything is a function called from main at the bottom, because a script
# delivered through a pipe is executed as it arrives: a truncated download must
# not be able to run half of an install.

set -eu

# repo is the release source. It is a variable only so the tests can point at
# another copy; there is nothing to configure here.
repo="${PIMCTL_REPO:-larsakerlund/pimctl}"
api="https://api.github.com/repos/$repo"

# usage prints how to drive the script, including the environment variables
# that are the only way to change what it does.
usage() {
  cat <<EOF
install.sh — install a released pimctl binary

Usage:
  install.sh [-h]

It picks the archive for this machine's OS and architecture, verifies it
against the release's sha256 list, and installs the binary.

Environment:
  GITHUB_TOKEN        Optional. Switches to the GitHub API, which a private
                      repository needs. Without it the public release URLs are
                      used, and no API rate limit applies.
  PIMCTL_VERSION      Release to install, e.g. v0.1.1. Default: the latest.
  PIMCTL_INSTALL_DIR  Where the binary goes. Default: \$HOME/.local/bin.

Needs curl, tar and either shasum or sha256sum.
EOF
}

# die reports why the install stopped and exits. Nothing is installed before
# the checksum matches, so every die before that point leaves the machine as it
# was.
die() {
  printf 'install.sh: %s\n' "$1" >&2
  exit 1
}

# say reports progress on stdout, where the person who ran the pipeline sees it.
say() {
  printf '%s\n' "$1"
}

# need_tool exits unless the command is available, naming it rather than
# letting the failure surface as "not found" from somewhere deeper.
need_tool() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required and was not found"
}

# resolve_token sets token to the credential to send, or to nothing.
#
# Installing a public release needs none, and the paths below go around the
# GitHub API so that it stays true: unauthenticated API calls are capped at sixty
# an hour per address, and a hosted CI runner shares its address with every other
# job on the platform. A token switches to the API, which a repository that
# requires a credential has no alternative to.
#
# Any command this script runs takes its stdin from /dev/null, because when the
# script arrives through a pipe its own remaining text is on stdin, and a child
# that read a line of it would eat part of the install.
resolve_token() {
  token="${GITHUB_TOKEN:-}"
}

# detect_platform sets os and arch to the pair that names a release archive.
#
# The names uname reports are not the names the archives use, and they differ
# per machine for the same chip: aarch64 and arm64 are one architecture, x86_64
# and amd64 another.
detect_platform() {
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$os" in
  darwin | linux) ;;
  *) die "no release for $(uname -s); pimctl ships darwin and linux builds" ;;
  esac

  arch=$(uname -m)
  case "$arch" in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64) arch=amd64 ;;
  *) die "no release for $arch; pimctl ships arm64 and amd64 builds" ;;
  esac
}

# make_workspace creates the private directory every download lands in and
# arranges for it to go away, whether the script finishes or fails.
#
# A token, when there is one, is written into a curl config file there rather
# than passed as an argument: argv is readable by every process on the machine
# through ps(1), and a 0600 file in a 0700 directory is not. Without a token the
# file is empty, so every request goes out the same way and there is one path
# through the code.
make_workspace() {
  work=$(mktemp -d "${TMPDIR:-/tmp}/pimctl-install.XXXXXX") ||
    die "cannot create a temporary directory"
  trap 'rm -rf "$work"' EXIT HUP INT TERM

  curl_conf="$work/curl.conf"
  : >"$curl_conf"
  chmod 600 "$curl_conf"
  if [ -n "$token" ]; then
    printf 'header = "Authorization: Bearer %s"\n' "$token" >"$curl_conf"
  fi
}

# trace prints the URL about to be requested when PIMCTL_TRACE is set.
#
# scripts/test-install.sh reads this to prove the tokenless path never reaches
# api.github.com. Only URLs are printed: the token lives in a config file and is
# never part of one.
trace() {
  if [ -n "${PIMCTL_TRACE:-}" ]; then
    printf 'trace: GET %s\n' "$1" >&2
  fi
}

# http_get fetches a URL into a file and sets status to the HTTP status code.
#
# Arguments: URL, the Accept header to send, the file to write. The status is
# returned rather than acted on because what a 404 means depends on what was
# being asked for.
http_get() {
  trace "$1"
  status=$(curl -sSL --config "$curl_conf" \
    --header "Accept: $2" \
    --header "X-GitHub-Api-Version: 2022-11-28" \
    --retry 2 --connect-timeout 15 --max-time 600 \
    --output "$3" --write-out '%{http_code}' "$1") ||
    die "cannot reach $1"
}

# resolve_release sets tag to the release that will be installed.
#
# Three ways in. PIMCTL_VERSION names the tag outright and costs no request at
# all. Without it, the tag comes from where /releases/latest redirects — a plain
# github.com request, outside the API and its rate limit. A token switches to the
# API, because a token is the only reason to prefer it: a repository that
# requires a credential serves its assets through the API and nowhere else.
resolve_release() {
  if [ -n "$token" ]; then
    fetch_release_json
    return
  fi
  if [ -n "${PIMCTL_VERSION:-}" ]; then
    tag="$PIMCTL_VERSION"
    return
  fi
  resolve_tag_from_redirect
}

# resolve_tag_from_redirect sets tag from where the "latest release" page sends
# a browser.
#
# GitHub answers https://github.com/<repo>/releases/latest with a 302 to
# .../releases/tag/<tag>: the tag name, with nothing to parse around it. A
# repository with no releases redirects to .../releases instead, so the shape is
# checked rather than the last path element taken on faith.
resolve_tag_from_redirect() {
  latest="https://github.com/$repo/releases/latest"
  trace "$latest"
  redirect=$(curl -sS --config "$curl_conf" \
    --retry 2 --connect-timeout 15 --max-time 60 \
    --output /dev/null --write-out '%{redirect_url}' "$latest") ||
    die "cannot reach $latest"
  case "$redirect" in
  */releases/tag/*) tag="${redirect##*/}" ;;
  *) die "no release to install: $latest did not lead to one" ;;
  esac
}

# fetch_release_json writes the release metadata to $work/release.json and sets
# tag to the release it describes. It is the token's path: only the API knows an
# asset's id, and only an asset id downloads from a repository that requires a
# credential.
fetch_release_json() {
  if [ -n "${PIMCTL_VERSION:-}" ]; then
    release_url="$api/releases/tags/$PIMCTL_VERSION"
  else
    release_url="$api/releases/latest"
  fi

  http_get "$release_url" "application/vnd.github+json" "$work/release.json"
  case "$status" in
  200) ;;
  401 | 403)
    die "GitHub refused the request (HTTP $status). If that is the rate limit, set GITHUB_TOKEN and try again"
    ;;
  404)
    die "no release found at $release_url (a request that is not allowed to see it reports 404 too)"
    ;;
  *) die "cannot read the release list (HTTP $status)" ;;
  esac

  tag=$(json_string tag_name <"$work/release.json")
  [ -n "$tag" ] || die "the release has no tag_name; cannot tell what to install"
}

# json_string prints the first value of a string field, reading JSON on stdin.
#
# Splitting on commas first puts one field on a line, so the same expression
# reads the API's pretty-printed JSON and a compact copy alike.
json_string() {
  tr ',' '\n' |
    sed -n 's/^[[:space:]]*"'"$1"'":[[:space:]]*"\([^"]*\)".*/\1/p' |
    head -n 1
}

# asset_id prints the release asset id for a file name in the release.
#
# Assets are downloaded by id: the browser_download_url needs a browser
# session, and the API's asset URL is the form a token can use. Each asset
# object lists its url before its name, so the id last seen belongs to the name
# on the line that matches.
asset_id() {
  tr ',' '\n' <"$work/release.json" | awk -v want="$1" '
    {
      if (match($0, /releases\/assets\/[0-9]+/)) {
        id = substr($0, RSTART, RLENGTH)
        sub(/releases\/assets\//, "", id)
      }
      if (index($0, "\"name\": \"" want "\"") ||
          index($0, "\"name\":\"" want "\"")) {
        if (id != "") { print id; exit }
      }
    }
  '
}

# fetch_asset downloads one release asset by id into a file.
#
# The octet-stream Accept header is what makes the API return the file itself
# rather than a description of it.
fetch_asset() {
  http_get "$api/releases/assets/$1" "application/octet-stream" "$2"
  [ "$status" = 200 ] || die "cannot download $3 (HTTP $status)"
}

# download_from_release_url fetches one of a release's files by name.
#
# https://github.com/<repo>/releases/download/<tag>/<file> is the public path,
# served through GitHub's CDN rather than the API, which is the whole point: no
# rate limit to run into and no credential to need.
download_from_release_url() {
  url="https://github.com/$repo/releases/download/$tag/$1"
  http_get "$url" "application/octet-stream" "$2"
  [ "$status" = 200 ] || die "cannot download $1 from release $tag (HTTP $status)"
}

# download_via_api fetches the same two files through the API, by asset id.
download_via_api() {
  archive_id=$(asset_id "$archive")
  [ -n "$archive_id" ] || die "release $tag has no $archive"
  sums_id=$(asset_id checksums.txt)
  [ -n "$sums_id" ] ||
    die "release $tag has no checksums.txt, so nothing can be verified"

  fetch_asset "$archive_id" "$work/$archive" "$archive"
  fetch_asset "$sums_id" "$work/checksums.txt" checksums.txt
}

# download_archive fetches the archive for this platform and the release's
# checksum list, setting archive to the archive's file name.
download_archive() {
  archive="pimctl_${tag#v}_${os}_${arch}.tar.gz"

  say "downloading $archive ($tag)"
  if [ -n "$token" ]; then
    download_via_api
  else
    download_from_release_url "$archive" "$work/$archive"
    download_from_release_url checksums.txt "$work/checksums.txt"
  fi

  # The one test hook. scripts/test-install.sh sets it to prove the
  # checksum gate refuses a tampered download. It can only make the
  # script refuse: there is no value of it that installs anything.
  if [ -n "${PIMCTL_TEST_CORRUPT:-}" ]; then
    printf 'tampered' >>"$work/$archive"
  fi
}

# detect_sha_tool sets sha_tool to the command that prints sha256 sums — macOS
# ships shasum, most Linux distributions sha256sum.
#
# It runs before anything is downloaded, so a machine with neither is told so
# immediately. It also keeps the decision out of [sha256_of], which is called
# inside a command substitution, where a die would exit that subshell and leave
# the script to carry on with an empty sum.
detect_sha_tool() {
  if command -v shasum >/dev/null 2>&1; then
    sha_tool=shasum
  elif command -v sha256sum >/dev/null 2>&1; then
    sha_tool=sha256sum
  else
    die "neither shasum nor sha256sum is installed; cannot verify the download"
  fi
}

# sha256_of prints the sha256 of a file.
sha256_of() {
  if [ "$sha_tool" = shasum ]; then
    shasum -a 256 "$1" | cut -d' ' -f1
  else
    sha256sum "$1" | cut -d' ' -f1
  fi
}

# verify_archive refuses to go on unless the archive matches the checksum the
# release published for it.
#
# This is the whole reason the checksum list is downloaded: a binary that
# arrived over the network is installed only once it is the binary the release
# says it is.
verify_archive() {
  want_sum=$(awk -v want="$archive" '$2 == want { print $1; exit }' \
    "$work/checksums.txt")
  [ -n "$want_sum" ] || die "checksums.txt does not list $archive"

  got_sum=$(sha256_of "$work/$archive")
  if [ "$want_sum" != "$got_sum" ]; then
    die "checksum mismatch for $archive: expected $want_sum, got $got_sum; nothing was installed"
  fi
  say "sha256 ok"
}

# install_binary unpacks the archive and puts the binary in place.
#
# The binary is staged inside the install directory and renamed over any
# existing one: a rename within a directory is atomic, so an interrupted
# install cannot leave a half-written pimctl on the PATH, and it replaces a
# copy that is currently running instead of failing with ETXTBSY.
install_binary() {
  tar -xzf "$work/$archive" -C "$work" ||
    die "cannot unpack $archive"
  [ -f "$work/pimctl" ] || die "$archive does not contain a pimctl binary"

  install_dir="${PIMCTL_INSTALL_DIR:-${HOME:-}/.local/bin}"
  [ "$install_dir" != "/.local/bin" ] ||
    die "\$HOME is not set; point PIMCTL_INSTALL_DIR at a directory instead"
  mkdir -p "$install_dir" || die "cannot create $install_dir"

  staged="$install_dir/.pimctl.install.$$"
  cp "$work/pimctl" "$staged" || die "cannot write to $install_dir"
  chmod 0755 "$staged" || {
    rm -f "$staged"
    die "cannot make $staged executable"
  }
  mv "$staged" "$install_dir/pimctl" || {
    rm -f "$staged"
    die "cannot install into $install_dir"
  }
}

# report says what was installed, proves it runs by asking it its version, and
# mentions the two things a fresh install still needs: a PATH that includes it,
# and shell completion, which the archive cannot install on anyone's behalf.
report() {
  say "installed $install_dir/pimctl"
  "$install_dir/pimctl" version </dev/null ||
    die "the installed binary does not run"

  case ":${PATH:-}:" in
  *":$install_dir:"*) ;;
  *)
    say ""
    say "$install_dir is not on your PATH. Add it:"
    say "    export PATH=\"$install_dir:\$PATH\""
    ;;
  esac
  say ""
  say "for zsh completion:"
  say "    pimctl completion zsh > ~/.local/share/zsh/site-functions/_pimctl"
}

# main runs the install in the order the steps depend on each other, and where
# two orders would both work it checks first what a machine is most likely to be
# missing: the arguments and the token, then the tools, then the platform — all
# of which fail without touching anything — then the download, the verification,
# and only then the install.
main() {
  case "${1:-}" in
  -h | --help)
    usage
    exit 0
    ;;
  "") ;;
  *)
    printf 'install.sh: unknown argument: %s\n\n' "$1" >&2
    usage >&2
    exit 1
    ;;
  esac

  need_tool curl
  need_tool tar
  resolve_token
  detect_sha_tool
  detect_platform
  make_workspace
  resolve_release
  download_archive
  verify_archive
  install_binary
  report
}

main "$@"
