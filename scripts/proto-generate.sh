#!/usr/bin/env bash
# Regenerates gen/ from this repo's proto/ and from the callee protos pinned in
# proto-refs.env. The callee protos are fetched into .protos/ (git-ignored) and
# never committed; only the generated stubs are.
#
# STEWARD_AUDIT_PROTO_DIR points at a local proto/ directory instead, for
# trying an unmerged proto change.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=/dev/null
source "$root/proto-refs.env"
# shellcheck source=/dev/null
source "$root/scripts/proto-lib.sh"
check_plugin_versions

protos="$root/.protos"
rm -rf "$protos" "$root/gen/go/thirdparty"

# fetch <repo> <ref> <local proto dir> <proto package dir>...
fetch() {
  local repo="$1" ref="$2" local_dir="$3"
  shift 3
  local dest="$protos/$repo" pkg archive
  mkdir -p "$dest"
  if [[ -n "$local_dir" ]]; then
    echo "proto: $repo from $local_dir"
    for pkg in "$@"; do
      mkdir -p "$dest/$pkg"
      cp -R "$local_dir/$pkg/." "$dest/$pkg/"
    done
    return
  fi
  echo "proto: $repo at $ref"
  archive="$(mktemp)"
  curl -sSfL "https://codeload.github.com/Steward-GRC/$repo/tar.gz/$ref" -o "$archive"
  extract_protos "$archive" "$dest" "$@"
  rm -f "$archive"
}

fetch steward-audit "$STEWARD_AUDIT_REF" "${STEWARD_AUDIT_PROTO_DIR:-}" steward/audit

cd "$root"
buf generate
