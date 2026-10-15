#!/usr/bin/env bash
# harnessctl-server statically links the Go standard library and third-party Go
# modules, so every build of the binary, and the image Dockerfile builds, redistributes
# them. third_party/licenses/go/ carries the licence files each of those modules ships,
# under its module path ("std" for the standard library), and modules.tsv records each
# module's version and SPDX licence expression.
#
#   scripts/go-module-licenses.sh check    fails when the linked modules, their versions
#                                          or their licence files drift from what is
#                                          committed, or a licence file lacks the text
#                                          of a licence modules.tsv records
#   scripts/go-module-licenses.sh update   rewrites third_party/licenses/go/ from the
#                                          module cache; review the diff, then commit it
#
# A module's licence files are those named LICENSE, LICENCE, COPYING, NOTICE or
# PATENTS, with any suffix, in the directory of a package the binary links or in a
# directory above it up to the module root. update keeps the licence modules.tsv
# records for a module whose version is unchanged, so a corrected entry survives. For
# a new module or version it records each licence whose text scripts/licence-text.sh
# recognises, or UNKNOWN, which check rejects until someone reviews the module.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
# shellcheck source=scripts/licence-text.sh
. scripts/licence-text.sh

OUT=third_party/licenses/go
MANIFEST="$OUT/modules.tsv"
# Dockerfile builds a static (CGO_ENABLED=0) linux binary for the host's platform.
PLATFORMS="linux/amd64 linux/arm64"
LICENCE_FILE='^(licen[cs]e|copying|notice|patents)([.-][a-z0-9]+)*$'

mode="${1:-check}"
case "$mode" in
  check | update) ;;
  *)
    echo "usage: $0 [check|update]" >&2
    exit 2
    ;;
esac
command -v go >/dev/null || {
  echo "go-module-licenses: missing dependency: go" >&2
  exit 1
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
status=0

# linked_files prints "module<TAB>version<TAB>module dir<TAB>licence file" for each
# licence file of each module the binary links, the file relative to the module dir.
# Every module also gets one line with an empty last field, so a module with no
# licence file still shows up.
linked_files() {
  local platform mod ver mdir pdir d f toolchain goroot
  for platform in $PLATFORMS; do
    GOOS="${platform%/*}" GOARCH="${platform#*/}" CGO_ENABLED=0 go list -deps \
      -f '{{if and .Module (not .Module.Main)}}{{.Module.Path}}{{"\t"}}{{.Module.Version}}{{"\t"}}{{.Module.Dir}}{{"\t"}}{{.Dir}}{{end}}' \
      ./cmd/harnessctl-server
  done | LC_ALL=C sort -u >"$tmp/packages"

  shopt -s nocasematch
  while IFS=$'\t' read -r mod ver mdir pdir; do
    [ -n "$mod" ] || continue
    printf '%s\t%s\t%s\t\n' "$mod" "$ver" "$mdir"
    d="$pdir"
    while :; do
      for f in "$d"/*; do
        [ -f "$f" ] && [[ "${f##*/}" =~ $LICENCE_FILE ]] && [[ "$f" != *.go ]] || continue
        printf '%s\t%s\t%s\t%s\n' "$mod" "$ver" "$mdir" "${f#"$mdir"/}"
      done
      case "$d" in "$mdir"/*) d="$(dirname "$d")" ;; *) break ;; esac
    done
  done <"$tmp/packages"
  shopt -u nocasematch

  # The standard library's licence is in GOROOT; record the toolchain go.mod names.
  toolchain="$(awk '$1 == "toolchain" { print $2 }' go.mod)"
  [ -n "$toolchain" ] || toolchain="go$(awk '$1 == "go" { print $2 }' go.mod)"
  goroot="$(go env GOROOT)"
  printf 'std\t%s\t%s\t\n' "$toolchain" "$goroot"
  for f in LICENSE PATENTS; do
    [ -f "$goroot/$f" ] && printf 'std\t%s\t%s\t%s\n' "$toolchain" "$goroot" "$f"
  done
}

# generate <dir> writes, into <dir>, the licence files and modules.tsv for the modules
# the binary links now.
generate() {
  local dest="$1" mod ver mdir rel files licence target
  mkdir -p "$dest"
  linked_files | LC_ALL=C sort -u >"$tmp/files"
  {
    echo "# Go modules linked into harnessctl-server, with their licence files under this"
    echo "# directory. Written by scripts/go-module-licenses.sh update; make licenses checks it."
    printf '# module\tversion\tlicence (SPDX)\tlicence files\n'
    while IFS=$'\t' read -r mod ver mdir; do
      files="$(awk -F'\t' -v m="$mod" '$1 == m && $4 != "" { print $4 }' "$tmp/files" | tr '\n' ' ')"
      files="${files% }"
      if [ -z "$files" ]; then
        echo "go-module-licenses: $mod $ver ships no licence file; find its licence before it ships" >&2
        status=1
        continue
      fi
      for rel in $files; do
        target="$dest/$mod/$rel"
        if [ -e "$target" ]; then
          echo "go-module-licenses: $target belongs to two modules" >&2
          exit 1
        fi
        mkdir -p "$(dirname "$target")"
        cat "$mdir/$rel" >"$target"
      done
      licence=""
      if [ -f "$MANIFEST" ]; then
        licence="$(awk -F'\t' -v m="$mod" -v v="$ver" '$1 == m && $2 == v { print $3 }' "$MANIFEST")"
      fi
      if [ -z "$licence" ]; then
        # shellcheck disable=SC2086 # files is a space-separated list of plain paths
        licence="$(cd "$dest/$mod" && detect_licences $files)"
      fi
      printf '%s\t%s\t%s\t%s\n' "$mod" "$ver" "$licence" "$files"
    done < <(cut -f1-3 "$tmp/files" | LC_ALL=C sort -u | awk -F'\t' '$1 == "std" { print; next } { rest[++n] = $0 } END { for (i = 1; i <= n; i++) print rest[i] }')
  } >"$dest/modules.tsv"
}

# licence_text_gaps reports each licence modules.tsv records whose text the module's
# committed licence files lack.
licence_text_gaps() {
  local mod ver licence files rel paths gap
  while IFS=$'\t' read -r mod ver licence files; do
    case "$mod" in '' | '#'*) continue ;; esac
    paths=""
    for rel in $files; do
      [ ! -f "$OUT/$mod/$rel" ] || paths="$paths $OUT/$mod/$rel"
    done
    [ -n "$paths" ] || continue # check reports the missing files
    # shellcheck disable=SC2086 # paths is a space-separated list of plain paths
    while read -r gap; do
      [ -n "$gap" ] || continue
      if licence_known "$gap"; then
        echo "go-module-licenses: $mod records \"$licence\" but $OUT/$mod/ has no $gap text" >&2
      else
        echo "go-module-licenses: $mod records \"$licence\"; $gap is not a licence scripts/licence-text.sh recognises, so review the module before it ships" >&2
      fi
      status=1
    done < <(licence_gaps "$licence" $paths)
  done <"$MANIFEST"
}

if [ "$mode" = update ]; then
  generate "$tmp/go"
  [ "$status" -eq 0 ] || exit "$status"
  rm -rf "$OUT"
  mv "$tmp/go" "$OUT"
  licence_text_gaps
  echo "go-module-licenses: wrote $OUT; review the diff before committing it"
  exit "$status"
fi

if [ ! -f "$MANIFEST" ]; then
  echo "go-module-licenses: $MANIFEST is missing; run scripts/go-module-licenses.sh update" >&2
  exit 1
fi
generate "$tmp/go"

if ! diff -u --label "$MANIFEST (committed)" --label "$MANIFEST (linked now)" "$MANIFEST" "$tmp/go/modules.tsv" >"$tmp/manifest.diff"; then
  echo "go-module-licenses: $MANIFEST does not match the modules harnessctl-server links; run scripts/go-module-licenses.sh update:" >&2
  sed -n 's/^\([-+][^-+]\)/  \1/p' "$tmp/manifest.diff" >&2
  status=1
fi

(cd "$OUT" && find . -type f ! -path ./modules.tsv | sed 's#^\./##' | LC_ALL=C sort) >"$tmp/committed"
(cd "$tmp/go" && find . -type f ! -path ./modules.tsv | sed 's#^\./##' | LC_ALL=C sort) >"$tmp/expected"
while read -r rel; do
  [ -z "$rel" ] || {
    echo "go-module-licenses: $OUT/$rel is missing" >&2
    status=1
  }
done < <(comm -13 "$tmp/committed" "$tmp/expected")
while read -r rel; do
  [ -z "$rel" ] || {
    echo "go-module-licenses: $OUT/$rel is not a licence file of a linked module" >&2
    status=1
  }
done < <(comm -23 "$tmp/committed" "$tmp/expected")
while read -r rel; do
  [ -z "$rel" ] || cmp -s "$OUT/$rel" "$tmp/go/$rel" || {
    echo "go-module-licenses: $OUT/$rel differs from the module's own copy" >&2
    status=1
  }
done < <(comm -12 "$tmp/committed" "$tmp/expected")

licence_text_gaps

if [ "$status" -eq 0 ]; then
  echo "go-module-licenses: $(grep -vc '^#' "$MANIFEST") linked Go module(s), standard library included, all attributed"
fi
exit "$status"
