# shellcheck shell=bash
# Sourced by the licence checks: tests whether checked-in licence files carry the text
# of each licence a dependency declares. Upstream files wrap the same text at
# different widths, so matching runs on whitespace-collapsed content.
#
# Every licence recognised here is permissive. A dependency under any other licence
# fails the checks until someone reviews it and adds its text here.

# has_licence_text <spdx-id> <file>...: succeeds when the files carry that licence's
# text. Returns 2 for an id this file does not recognise.
has_licence_text() {
  local id="$1" text
  shift
  text="$(cat -- "$@" | tr -s '[:space:]' ' ')"
  case "$id" in
    Apache-2.0) grep -qE 'Apache License,? Version 2\.0' <<<"$text" ;;
    BSD-2-Clause) grep -qF 'Redistribution and use in source and binary forms' <<<"$text" ;;
    # The third clause, the endorsement ban, is what BSD-2-Clause text lacks.
    BSD-3-Clause)
      grep -qF 'Redistribution and use in source and binary forms' <<<"$text" &&
        grep -qF 'used to endorse or promote products' <<<"$text"
      ;;
    ISC) grep -qF 'Permission to use, copy, modify, and/or distribute this software for any purpose' <<<"$text" ;;
    MIT) grep -qF 'Permission is hereby granted, free of charge, to any person obtaining a copy' <<<"$text" ;;
    *) return 2 ;;
  esac
}

# The ids has_licence_text recognises.
LICENCE_IDS="Apache-2.0 BSD-2-Clause BSD-3-Clause ISC MIT"

# licence_known <spdx-id>: succeeds when has_licence_text recognises the id.
licence_known() {
  case " $LICENCE_IDS " in
    *" $1 "*) return 0 ;;
    *) return 1 ;;
  esac
}

# detect_licences <file>...: prints an SPDX expression joining, with AND, each licence
# whose text the files carry, or UNKNOWN when they carry none it recognises.
detect_licences() {
  local id found=""
  for id in $LICENCE_IDS; do
    has_licence_text "$id" "$@" || continue
    # BSD-3-Clause text carries the BSD-2-Clause wording too.
    if [ "$id" = BSD-2-Clause ] && has_licence_text BSD-3-Clause "$@"; then
      continue
    fi
    found="${found:+$found AND }$id"
  done
  echo "${found:-UNKNOWN}"
}

# licence_gaps <spdx-expression> <file>...: prints, one per line, each licence the
# expression needs that the files do not carry, and nothing when they carry enough.
# "A AND B" needs both texts; "A OR B" needs either. AND binds tighter than OR and
# parentheses are dropped, which is exact for the flat forms packages declare. An
# "A WITH exception" counts as A.
licence_gaps() {
  local expr="$1" tok id skip=0 ids="" gaps n best="" best_n=-1
  shift
  # A trailing OR closes the last alternative.
  for tok in $(printf '%s OR\n' "$expr" | tr '()' '  '); do
    if [ "$skip" = 1 ]; then
      skip=0
      continue
    fi
    case "$tok" in
      AND) ;;
      WITH) skip=1 ;;
      OR)
        [ -n "$ids" ] || continue
        gaps=""
        n=0
        for id in $ids; do
          has_licence_text "$id" "$@" && continue
          gaps="$gaps$id"$'\n'
          n=$((n + 1))
        done
        [ "$n" -gt 0 ] || return 0
        if [ "$best_n" -lt 0 ] || [ "$n" -lt "$best_n" ]; then
          best="$gaps"
          best_n="$n"
        fi
        ids=""
        ;;
      *) ids="$ids $tok" ;;
    esac
  done
  if [ "$best_n" -lt 0 ]; then
    echo "(no licence declared)"
  else
    printf '%s' "$best"
  fi
}
