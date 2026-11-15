#!/usr/bin/env bash
# Copyright 2026 The Steward Authors
# SPDX-License-Identifier: Apache-2.0
#
# Helpers for proto-generate.sh, kept apart so scripts/proto-generate_test.sh
# can run them without the network. Source this file; it sets no shell options.

# Pinned protoc plugin versions. A different version rewrites the header of
# every generated file, so proto-generate.sh stops instead.
PROTOC_GEN_GO_VERSION="${PROTOC_GEN_GO_VERSION:-v1.36.12}"
PROTOC_GEN_GO_GRPC_VERSION="${PROTOC_GEN_GO_GRPC_VERSION:-v1.6.2}"

# extract_protos <archive.tar.gz> <dest> <proto package dir>...
# Extracts the whole archive into a temporary directory and copies each
# <top>/proto/<pkg>/ folder to <dest>/<pkg>/. It needs no GNU-only tar option
# (--wildcards), so it runs the same with GNU tar and the BSD tar of macOS.
extract_protos() {
  local archive="$1" dest="$2" tmp pkg src
  shift 2
  tmp="$(mktemp -d)"
  tar -xzf "$archive" -C "$tmp"
  for pkg in "$@"; do
    src=""
    for src in "$tmp"/*/proto/"$pkg"; do
      [[ -d "$src" ]] && break
      src=""
    done
    if [[ -z "$src" ]]; then
      rm -rf "$tmp"
      echo "proto: package $pkg not found in $archive" >&2
      return 1
    fi
    mkdir -p "$dest/$pkg"
    cp -R "$src/." "$dest/$pkg/"
  done
  rm -rf "$tmp"
}

# check_plugin_versions stops with a clear message when protoc-gen-go or
# protoc-gen-go-grpc is missing or not the pinned version.
check_plugin_versions() {
  local got
  got="$(protoc-gen-go --version 2>/dev/null | awk '{print $2}')" || got=""
  if [[ "$got" != "$PROTOC_GEN_GO_VERSION" ]]; then
    echo "proto: protoc-gen-go is '${got:-missing}', need $PROTOC_GEN_GO_VERSION" >&2
    echo "  go install google.golang.org/protobuf/cmd/protoc-gen-go@$PROTOC_GEN_GO_VERSION" >&2
    return 1
  fi
  got="$(protoc-gen-go-grpc --version 2>/dev/null | awk '{print $2}')" || got=""
  got="v${got#v}"
  if [[ "$got" != "$PROTOC_GEN_GO_GRPC_VERSION" ]]; then
    echo "proto: protoc-gen-go-grpc is '${got#v}', need $PROTOC_GEN_GO_GRPC_VERSION" >&2
    echo "  go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$PROTOC_GEN_GO_GRPC_VERSION" >&2
    return 1
  fi
}
