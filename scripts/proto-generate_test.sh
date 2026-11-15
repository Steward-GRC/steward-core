#!/usr/bin/env bash
# Copyright 2026 The Steward Authors
# SPDX-License-Identifier: Apache-2.0
#
# Tests for scripts/proto-lib.sh. Runs offline with the system tar (GNU or BSD).
# shellcheck disable=SC2329 # the t_* functions run through check
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
source "$here/proto-lib.sh"

fail=0
check() { # name, command...
  local name="$1"
  shift
  if "$@"; then echo "ok   $name"; else echo "FAIL $name"; fail=1; fi
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# A GitHub-style archive: one top folder, proto/<pkg>/ inside, plus noise.
mkdir -p "$work/src/steward-audit-abc123/proto/steward/audit/v1" "$work/src/steward-audit-abc123/proto/other" "$work/src/steward-audit-abc123/cmd"
echo 'syntax = "proto3";' >"$work/src/steward-audit-abc123/proto/steward/audit/v1/audit.proto"
echo x >"$work/src/steward-audit-abc123/proto/other/skip.proto"
echo x >"$work/src/steward-audit-abc123/cmd/main.go"
tar -czf "$work/archive.tgz" -C "$work/src" steward-audit-abc123

t_extract() {
  extract_protos "$work/archive.tgz" "$work/dest" steward/audit || return 1
  [[ -f "$work/dest/steward/audit/v1/audit.proto" ]] || return 1
  [[ ! -e "$work/dest/other" && ! -e "$work/dest/cmd" ]]
}
check "extract_protos copies only the wanted package" t_extract

t_missing() { ! extract_protos "$work/archive.tgz" "$work/dest2" steward/nope 2>/dev/null; }
check "extract_protos fails on a missing package" t_missing

# Fake plugins on PATH.
fake() { # dir go-version grpc-version
  mkdir -p "$1"
  printf '#!/bin/sh\necho "protoc-gen-go %s"\n' "$2" >"$1/protoc-gen-go"
  printf '#!/bin/sh\necho "protoc-gen-go-grpc %s"\n' "$3" >"$1/protoc-gen-go-grpc"
  chmod +x "$1/protoc-gen-go" "$1/protoc-gen-go-grpc"
}
fake "$work/good" v1.36.12 1.6.2
fake "$work/badgo" v1.36.0 1.6.2
fake "$work/badgrpc" v1.36.12 1.5.0

check "pinned plugin versions pass" env PATH="$work/good:$PATH" bash -c "source '$here/proto-lib.sh'; check_plugin_versions"
t_badgo() { ! PATH="$work/badgo:$PATH" bash -c "source '$here/proto-lib.sh'; check_plugin_versions" 2>"$work/err" && grep -q "protoc-gen-go is 'v1.36.0'" "$work/err"; }
check "wrong protoc-gen-go stops with a message" t_badgo
t_badgrpc() { ! PATH="$work/badgrpc:$PATH" bash -c "source '$here/proto-lib.sh'; check_plugin_versions" 2>"$work/err" && grep -q "protoc-gen-go-grpc is '1.5.0'" "$work/err"; }
check "wrong protoc-gen-go-grpc stops with a message" t_badgrpc

exit "$fail"
