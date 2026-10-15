#!/usr/bin/env bash
# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT
#
# Runs every example under recommendations/examples and compares its output to its README.
# Contributors run it before a pull request. For each examples/<name>/README.md it extracts the first ```sh
# block (the command) and the last ```text block (the expected output), runs the command from the
# repository root, and diffs. It also fails if the index lists a folder that does not exist or misses
# one that does.
#
# An example whose folder holds a `.needs-postgres` marker needs a database: the runner creates a fresh
# scratch database from POSTGRES_TEST_DSN (a libpq conninfo that can reach an existing database, for
# example `host=localhost port=5432 user=postgres password=postgres dbname=postgres sslmode=disable`),
# runs the example against it, then drops it, also when the example fails or the run is interrupted.
# Without POSTGRES_TEST_DSN those examples are skipped with a message; the offline examples still run.
#
# Finding ids are assigned per scan, so the runner rewrites each finding-id line to `<finding-id>` before
# comparing; the READMEs show that token.
set -euo pipefail
cd "$(dirname "$0")/../.."

go vet ./recommendations/examples/... >/dev/null

examples_dir="recommendations/examples"
dirs="$(find "$examples_dir" -mindepth 1 -maxdepth 1 -type d | sort)"
indexed="$(grep -oE '\]\(([a-z0-9-]+)/\)' "$examples_dir/README.md" | sed -E 's/\]\(([a-z0-9-]+)\/\)/\1/' | sort -u)"

# Cross-check the index against disk, both directions.
for d in $dirs; do
	name="${d#"$examples_dir"/}"
	if ! printf '%s\n' "$indexed" | grep -qx "$name"; then
		echo "example $name is not listed in $examples_dir/README.md" >&2
		exit 1
	fi
done
for name in $indexed; do
	if [ ! -d "$examples_dir/$name" ]; then
		echo "$examples_dir/README.md lists $name, which does not exist" >&2
		exit 1
	fi
done

extract_block() { # <file> <lang> <which: first|last>
	awk -v lang="$2" -v which="$3" '
		$0 == "```"lang { inblk=1; buf=""; next }
		inblk && $0 == "```" { inblk=0; last=buf; if (which=="first" && !seen) { print buf; seen=1 } next }
		inblk { buf = buf $0 "\n" }
		END { if (which=="last") printf "%s", last }
	' "$1"
}

# normalize rewrites the per-scan finding-id line to a fixed token so the comparison is stable.
normalize() {
	sed -E 's/^    [0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}:.*/    <finding-id>/'
}

# drop_scratch drops the current example's scratch database, if one exists. It runs after each example
# and again on exit, so an example that fails, or an interrupted run, still leaves no database behind.
scratch=""
drop_scratch() {
	if [ -n "$scratch" ]; then
		psql "$POSTGRES_TEST_DSN" -q -c "DROP DATABASE IF EXISTS \"$scratch\"" >/dev/null 2>&1 || true
		scratch=""
	fi
}
trap drop_scratch EXIT
trap 'exit 130' INT TERM

fail=0
for d in $dirs; do
	name="${d#"$examples_dir"/}"
	readme="$d/README.md"
	cmd="$(extract_block "$readme" sh first)"
	want="$(extract_block "$readme" text last)"

	if [ -f "$d/.needs-postgres" ]; then
		if [ -z "${POSTGRES_TEST_DSN:-}" ]; then
			echo "skip $name: needs POSTGRES_TEST_DSN (a Postgres you can create a database on)"
			continue
		fi
		scratch="twh_ex_${name//-/_}_$$"
		psql "$POSTGRES_TEST_DSN" -v ON_ERROR_STOP=1 -q -c "CREATE DATABASE \"$scratch\"" >/dev/null
		example_dsn="$(printf '%s' "$POSTGRES_TEST_DSN" | sed -E "s/dbname=[^ ]+/dbname=$scratch/")"
		got="$(POSTGRES_TEST_DSN="$example_dsn" eval "$cmd" 2>/dev/null | normalize)"
		drop_scratch
	else
		got="$(eval "$cmd" 2>/dev/null | normalize)"
	fi

	if [ "$got" != "$want" ]; then
		echo "FAIL $name: output does not match its README" >&2
		diff <(printf '%s' "$want") <(printf '%s' "$got") >&2 || true
		fail=1
	else
		echo "ok   $name"
	fi
done
exit "$fail"
