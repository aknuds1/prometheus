#!/usr/bin/env bash
# Copyright The Prometheus Authors
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
makefile=${1:-"$repo_root/Makefile.common"}
makefile=$(cd "$(dirname "$makefile")" && pwd)/$(basename "$makefile")
test_make=${TEST_MAKE:-make}
test_tmp=$(mktemp -d)
trap 'rm -rf "$test_tmp"' EXIT

TEST_REAL_MKTEMP=$(command -v mktemp)
TEST_REAL_TAR=$(command -v tar)
TEST_REAL_INSTALL=$(command -v install)
TEST_REAL_MV=$(command -v mv)
TEST_HASH_TOOL=
for utility in gsha256sum sha256sum shasum openssl; do
  if TEST_HASH_TOOL=$(command -v "$utility"); then
    break
  fi
done
[[ -n "$TEST_HASH_TOOL" ]] || {
  echo 'No SHA-256 utility available for tests' >&2
  exit 1
}
export TEST_REAL_MKTEMP TEST_REAL_TAR TEST_REAL_INSTALL TEST_REAL_MV TEST_HASH_TOOL

mkdir "$test_tmp/bin"
cat > "$test_tmp/bin/mock" << 'MOCK'
#!/usr/bin/env bash
set -euo pipefail

case "${0##*/}" in
  test-sha256)
    case "${TEST_HASH_TOOL##*/}" in
      openssl)
        digest=$("$TEST_HASH_TOOL" dgst -sha256 "$1")
        printf '%s\n' "${digest##* }"
        ;;
      shasum)
        digest=$("$TEST_HASH_TOOL" -a 256 "$1")
        printf '%s\n' "${digest%% *}"
        ;;
      *)
        digest=$("$TEST_HASH_TOOL" "$1")
        printf '%s\n' "${digest%% *}"
        ;;
    esac
    ;;
  curl)
    output=
    url=
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --output|-o) output=$2; shift 2 ;;
        https://*) url=$1; shift ;;
        *) shift ;;
      esac
    done
    [[ -n "$url" && -n "$output" ]]
    printf '%s\n' "$url" >> "$TEST_CASE/downloads"
    if [[ $TEST_SCENARIO == download-failure ]]; then
      printf 'partial response' > "$output"
      echo 'Simulated download failure' >&2
      exit 22
    fi
    case "$url" in
      "$TEST_ARCHIVE_URL") source=$TEST_ARCHIVE ;;
      "$TEST_MANIFEST_URL") source=$TEST_CASE/manifest ;;
      "$TEST_INSTALLER_URL") source=$TEST_CASE/installer ;;
      *) echo "Unexpected download: $url" >&2; exit 1 ;;
    esac
    cp "$source" "$output"
    ;;
  mktemp)
    if [[ $TEST_SCENARIO == temporary-directory-failure ]]; then
      echo 'Simulated temporary directory failure' >&2
      exit 1
    fi
    path=$("$TEST_REAL_MKTEMP" "$@")
    printf '%s\n' "$path" >> "$TEST_CASE/temporary-paths"
    printf '%s\n' "$path"
    ;;
  tar)
    echo tar >> "$TEST_CASE/operations"
    if [[ $TEST_SCENARIO == extraction-failure ]]; then
      echo 'Simulated extraction failure' >&2
      exit 1
    fi
    "$TEST_REAL_TAR" "$@"
    ;;
  install)
    echo install >> "$TEST_CASE/operations"
    if [[ $TEST_SCENARIO == installation-failure || $TEST_SCENARIO == replacement-failure ]]; then
      printf 'partial executable' > "${!#}"
      echo 'Simulated installation failure' >&2
      exit 1
    fi
    "$TEST_REAL_INSTALL" "$@"
    ;;
  mv)
    echo mv >> "$TEST_CASE/operations"
    if [[ $TEST_SCENARIO == publication-failure ]]; then
      echo 'Simulated publication failure' >&2
      exit 1
    fi
    "$TEST_REAL_MV" "$@"
    ;;
esac
MOCK
chmod +x "$test_tmp/bin/mock"
for tool in test-sha256 curl mktemp tar install mv; do
  ln -s mock "$test_tmp/bin/$tool"
done

fail() {
  echo "FAIL: $TEST_SCENARIO ($platform): $*" >&2
  cat "$TEST_CASE/output" >&2
  exit 1
}

# Run the substitution first so the unfixed recipe fails for installing it.
while read -r TEST_SCENARIO host_os host_arch; do
  platform=$host_os-${host_arch/i386/386}
  TEST_CASE=$test_tmp/$TEST_SCENARIO-$host_os-$host_arch
  archive_dir=golangci-lint-2.12.2-$platform
  binary=$TEST_CASE/gopath/bin/golangci-lint
  mkdir -p "$TEST_CASE/source/$archive_dir" "$TEST_CASE/tmp"
  : > "$TEST_CASE/downloads"
  : > "$TEST_CASE/operations"
  : > "$TEST_CASE/temporary-paths"

  for payload in trusted substituted; do
    printf '#!/bin/sh\nprintf "%s\\n"\n' "$payload" > "$TEST_CASE/$payload"
    cp "$TEST_CASE/$payload" "$TEST_CASE/source/$archive_dir/golangci-lint"
    # The install step must set executable permissions independently of tar.
    chmod 0644 "$TEST_CASE/source/$archive_dir/golangci-lint"
    "$TEST_REAL_TAR" -czf "$TEST_CASE/$payload.tar.gz" -C "$TEST_CASE/source" "$archive_dir"
  done

  TEST_ARCHIVE_URL=https://github.com/golangci/golangci-lint/releases/download/v2.12.2/$archive_dir.tar.gz
  TEST_MANIFEST_URL=https://github.com/golangci/golangci-lint/releases/download/v2.12.2/golangci-lint-2.12.2-checksums.txt
  TEST_INSTALLER_URL=https://raw.githubusercontent.com/golangci/golangci-lint/v2.12.2/install.sh
  TEST_ARCHIVE=$TEST_CASE/trusted.tar.gz
  expected_hash=$("$test_tmp/bin/test-sha256" "$TEST_ARCHIVE")

  # This trusted fixture models the old installer's independently downloaded
  # archive and manifest. Its own pinned checksum remains valid during attack.
  cat > "$TEST_CASE/installer" << 'INSTALLER'
#!/bin/sh
set -eu
installer_tmp="$(mktemp -d)"
trap 'rm -rf "$installer_tmp"' EXIT
curl --output "$installer_tmp/archive.tar.gz" "$TEST_ARCHIVE_URL"
curl --output "$installer_tmp/manifest" "$TEST_MANIFEST_URL"
expected=$(awk '{print $1}' "$installer_tmp/manifest")
[ "$(test-sha256 "$installer_tmp/archive.tar.gz")" = "$expected" ]
tar -xzf "$installer_tmp/archive.tar.gz" -C "$installer_tmp"
install -m 755 "$installer_tmp/$TEST_ARCHIVE_DIR/golangci-lint" "$2/golangci-lint"
INSTALLER
  installer_hash=$("$test_tmp/bin/test-sha256" "$TEST_CASE/installer")

  success=false
  downloads=1
  extra_args=(--no-print-directory)
  error=
  case "$TEST_SCENARIO" in
    substituted)
      TEST_ARCHIVE=$TEST_CASE/substituted.tar.gz
      error='SHA-256 checksum mismatch'
      ;;
    valid | checksum-override | circle-enabled) success=true ;;
    missing-version)
      extra_args+=(GOLANGCI_LINT_VERSION=v0.0.0)
      error='No golangci-lint checksum configured'
      downloads=0
      ;;
    missing-checksum)
      extra_args+=(GOLANGCI_LINT_SHA256=)
      error='No golangci-lint checksum configured'
      downloads=0
      ;;
    temporary-directory-failure)
      error='Simulated temporary directory failure'
      downloads=0
      ;;
    download-failure) error='Simulated download failure' ;;
    extraction-failure) error='Simulated extraction failure' ;;
    installation-failure | replacement-failure) error='Simulated installation failure' ;;
    publication-failure) error='Simulated publication failure' ;;
    invalid-archive)
      printf 'not a tar archive\n' > "$TEST_ARCHIVE"
      expected_hash=$("$test_tmp/bin/test-sha256" "$TEST_ARCHIVE")
      ;;
    skipped | circle-skipped | unsupported)
      downloads=0
      success=true
      ;;
  esac
  case "$TEST_SCENARIO" in
    skipped) extra_args+=(SKIP_GOLANGCI_LINT=1) ;;
    circle-skipped)
      mkdir -p "$TEST_CASE/.github/workflows"
      touch "$TEST_CASE/.github/workflows/golangci-lint.yml"
      extra_args+=(CIRCLE_JOB=test)
      ;;
    circle-enabled) extra_args+=(CIRCLE_JOB=test) ;;
  esac
  if [[ $TEST_SCENARIO == checksum-override ]]; then
    extra_args+=("GOLANGCI_LINT_SHA256=$expected_hash")
    expected_hash=0000000000000000000000000000000000000000000000000000000000000000
  fi
  if [[ $TEST_SCENARIO == replacement-failure ]]; then
    mkdir -p "${binary%/*}"
    cp "$TEST_CASE/trusted" "$binary"
    chmod 0755 "$binary"
    extra_args+=(-B)
  fi
  printf '%s  %s.tar.gz\n' "$("$test_tmp/bin/test-sha256" "$TEST_ARCHIVE")" "$archive_dir" > "$TEST_CASE/manifest"

  export TEST_SCENARIO TEST_CASE TEST_ARCHIVE_URL TEST_MANIFEST_URL TEST_INSTALLER_URL TEST_ARCHIVE
  TEST_ARCHIVE_DIR=$archive_dir
  export TEST_ARCHIVE_DIR
  target=$binary
  case "$TEST_SCENARIO" in
    skipped | circle-skipped | unsupported) target=common-lint ;;
  esac
  status=0
  (
    # CI's skip flag and jobserver settings must not disable fixture installs.
    unset MAKEFLAGS MFLAGS MAKEOVERRIDES GOLANGCI_LINT_SHA256
    cd "$TEST_CASE"
    PATH="$test_tmp/bin:$PATH" TMPDIR="$TEST_CASE/tmp" "$test_make" -f "$makefile" \
      "SHELL=${TEST_SHELL:-/bin/sh}" \
      GO=true 'GO_VERSION=go version go1.26.0' "GOHOSTOS=$host_os" "GOHOSTARCH=$host_arch" \
      GOOS=windows GOARCH=arm GO_BUILD_PLATFORM=windows-armv7 \
      "FIRST_GOPATH=$TEST_CASE/gopath" DOCKERFILE_VARIANTS= DOCKER_IMAGE_TAG=test \
      SKIP_GOLANGCI_LINT= CIRCLE_JOB= GOLANGCI_LINT_VERSION=v2.12.2 \
      "GOLANGCI_LINT_SHA256_v2.12.2_$platform=$expected_hash" \
      "GOLANGCI_LINT_INSTALLER_SHA256=$installer_hash" \
      "${extra_args[@]}" "$target"
  ) > "$TEST_CASE/output" 2>&1 || status=$?

  if $success; then
    [[ $status == 0 ]] || fail "Expected success, got status $status"
  else
    if [[ $status == 0 ]]; then
      if [[ $TEST_SCENARIO == substituted ]] && cmp -s "$binary" "$TEST_CASE/substituted"; then
        fail 'Installed the substituted binary using its forged manifest'
      fi
      fail 'Expected failure'
    fi
    [[ -z "$error" ]] || grep -Fq "$error" "$TEST_CASE/output" || fail "Missing error: $error"
  fi

  if [[ $TEST_SCENARIO == valid || $TEST_SCENARIO == checksum-override || $TEST_SCENARIO == circle-enabled || $TEST_SCENARIO == replacement-failure ]]; then
    cmp -s "$binary" "$TEST_CASE/trusted" || fail 'Incorrect installed contents'
    [[ -x $binary ]] || fail 'Installed file is not executable'
  else
    [[ ! -e $binary ]] || fail 'Unexpected installed binary'
  fi
  [[ $(wc -l < "$TEST_CASE/downloads") -eq $downloads ]] || fail 'Incorrect number of downloads'
  if [[ $downloads == 1 ]]; then
    [[ $(cat "$TEST_CASE/downloads") == "$TEST_ARCHIVE_URL" ]] || fail 'Downloaded an installer or manifest'
  fi
  case "$TEST_SCENARIO" in
    substituted | missing-version | missing-checksum | temporary-directory-failure | download-failure | skipped | circle-skipped | unsupported)
      [[ ! -s $TEST_CASE/operations ]] || fail 'Used an unverified archive'
      ;;
    extraction-failure | invalid-archive)
      [[ $(cat "$TEST_CASE/operations") == tar ]] || fail 'Installed after extraction failure'
      ;;
  esac
  while IFS= read -r path; do
    [[ ! -e $path ]] || fail "Temporary path leaked: $path"
  done < "$TEST_CASE/temporary-paths"
  echo "PASS: $TEST_SCENARIO ($host_os/$host_arch)"
done << 'CASES'
substituted linux amd64
valid darwin amd64
valid darwin arm64
valid linux 386
valid linux i386
valid linux amd64
valid linux arm64
checksum-override linux amd64
missing-version linux amd64
missing-checksum linux amd64
temporary-directory-failure linux amd64
download-failure linux amd64
extraction-failure linux amd64
invalid-archive linux amd64
installation-failure linux amd64
publication-failure linux amd64
replacement-failure linux amd64
skipped linux amd64
circle-skipped linux amd64
circle-enabled linux amd64
unsupported windows amd64
unsupported freebsd amd64
CASES

TEST_REAL_MAKE=$(command -v "$test_make")
export TEST_REAL_MAKE
make_fixture=$test_tmp/make-fixture
mkdir -p "$make_fixture/bin" "$make_fixture/scripts"
ln -s "$repo_root/Makefile.common" "$make_fixture/Makefile.common"
printf '.PHONY: probe\nprobe:\n\t@:\n' > "$make_fixture/probe.mk"

# Wrappers also work with macOS's launcher, which cannot be renamed.
cat > "$make_fixture/bin/parent-make" << 'MAKE_WRAPPER'
#!/bin/sh
set -eu
printf '%s\n' "$0" >> "$TEST_CASE/make-invocations"
exec "$TEST_REAL_MAKE" "$@" MAKE="$0"
MAKE_WRAPPER
cp "$make_fixture/bin/parent-make" "$make_fixture/bin/override-make"
cat > "$make_fixture/bin/make" << 'WRONG_MAKE'
#!/bin/sh
echo 'Unexpected invocation of make from PATH' >&2
exit 1
WRONG_MAKE
chmod +x "$make_fixture/bin/parent-make" "$make_fixture/bin/override-make" "$make_fixture/bin/make"

# Exercise the real Makefile recipe without recursively running this suite.
cat > "$make_fixture/scripts/test-tool-downloads.sh" << 'MAKE_PROBE'
#!/bin/sh
set -eu
selected_make=${TEST_MAKE:-make}
printf '%s\n' "$selected_make" > "$TEST_CASE/selected-make"
unset MAKEFLAGS MFLAGS MAKEOVERRIDES
exec "$selected_make" --no-print-directory -f probe.mk probe
MAKE_PROBE
chmod +x "$make_fixture/scripts/test-tool-downloads.sh"

platform=$test_make
while read -r TEST_SCENARIO; do
  TEST_CASE=$test_tmp/$TEST_SCENARIO
  mkdir "$TEST_CASE"
  expected_make=$make_fixture/bin/parent-make
  extra_args=(--no-print-directory)
  case "$TEST_SCENARIO" in
    make-environment-override) expected_make=$make_fixture/bin/override-make ;;
    make-command-line-override | make-command-line-precedence)
      expected_make=$make_fixture/bin/override-make
      extra_args+=("TEST_MAKE=$expected_make")
      ;;
    make-parallel) extra_args+=(-j2) ;;
    make-dry-run) extra_args+=(-n) ;;
  esac
  status=0
  (
    # Inherited TEST_MAKE would hide a missing export in the target under test.
    unset MAKE MAKEFLAGS MFLAGS MAKEOVERRIDES MAKEFILES GNUMAKEFLAGS TEST_MAKE
    case "$TEST_SCENARIO" in
      make-environment-override) export TEST_MAKE=$make_fixture/bin/override-make ;;
      make-command-line-precedence) export TEST_MAKE=$make_fixture/bin/parent-make ;;
    esac
    cd "$make_fixture"
    PATH="$make_fixture/bin:$PATH" "$make_fixture/bin/parent-make" -f "$repo_root/Makefile" \
      "SHELL=${TEST_SHELL:-/bin/sh}" \
      GO=true 'GO_VERSION=go version go1.26.0' GOHOSTOS=linux GOHOSTARCH=amd64 \
      "FIRST_GOPATH=$TEST_CASE/gopath" DOCKERFILE_VARIANTS= DOCKER_IMAGE_TAG=test \
      SKIP_GOLANGCI_LINT=1 CIRCLE_JOB= "${extra_args[@]}" test-tool-downloads
  ) > "$TEST_CASE/output" 2>&1 || status=$?
  [[ $status == 0 ]] || fail "Expected success, got status $status"

  printf '%s\n' "$make_fixture/bin/parent-make" > "$TEST_CASE/expected-invocations"
  if [[ $TEST_SCENARIO == make-dry-run ]]; then
    [[ ! -e $TEST_CASE/selected-make ]] || fail 'Executed the test recipe during a dry run'
  else
    [[ -f $TEST_CASE/selected-make ]] || fail 'Test recipe did not run'
    [[ $(cat "$TEST_CASE/selected-make") == "$expected_make" ]] || fail 'Incorrect exported Make executable'
    printf '%s\n' "$expected_make" >> "$TEST_CASE/expected-invocations"
  fi
  cmp -s "$TEST_CASE/make-invocations" "$TEST_CASE/expected-invocations" || fail 'Incorrect Make invocations'
  echo "PASS: $TEST_SCENARIO"
done << 'CASES'
make-default
make-environment-override
make-command-line-override
make-command-line-precedence
make-parallel
make-dry-run
CASES
