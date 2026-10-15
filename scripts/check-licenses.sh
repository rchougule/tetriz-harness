#!/usr/bin/env bash
# The admin UI's production dependencies are redistributed: web/embed.go compiles
# web/dist into the harnessctl-server binary, so each one owes an attribution.
# This fails when that set drifts from what NOTICE names and third_party/licenses/
# carries, which a dependency bump does silently, and when a package's licence file
# lacks the text of a licence its package.json declares.
#
# Needs an installed workspace: run `pnpm install --frozen-lockfile` first.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
# shellcheck source=scripts/licence-text.sh
. scripts/licence-text.sh

for dep in node pnpm; do
  command -v "$dep" >/dev/null || { echo "check-licenses: missing dependency: $dep" >&2; exit 1; }
done

# Every transitive production dependency of web/, as "name<TAB>version<TAB>licence",
# the licence being the SPDX expression the installed package.json declares.
# Workspace links resolve to source in this repository and carry no third-party
# licence.
bundled="$(cd web && pnpm list --prod --depth Infinity --json | node -e '
const fs = require("fs");
const path = require("path");
const licenceOf = (dir) => {
  const pkg = JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8"));
  if (typeof pkg.license === "string") return pkg.license;
  if (pkg.license && pkg.license.type) return pkg.license.type;
  if (Array.isArray(pkg.licenses)) return pkg.licenses.map((l) => l.type || l).join(" OR ");
  return "";
};
let input = "";
process.stdin.on("data", (d) => (input += d)).on("end", () => {
  const seen = new Map();
  const walk = (deps) => {
    for (const [name, info] of Object.entries(deps || {})) {
      const version = info.version || "";
      if (version.startsWith("link:") || version.startsWith("workspace:")) continue;
      const key = `${name}@${version}`;
      if (seen.has(key)) continue;
      seen.set(key, [name, version, licenceOf(info.path)]);
      walk(info.dependencies);
    }
  };
  for (const project of JSON.parse(input)) walk(project.dependencies);
  for (const row of [...seen.values()].sort()) console.log(row.join("\t"));
});
')"

[ -n "$bundled" ] || { echo "check-licenses: no production dependencies resolved; is the workspace installed?" >&2; exit 1; }

status=0
while IFS=$'\t' read -r name version licence; do
  [ -n "$name" ] || continue
  dir="third_party/licenses/${name//\//-}"
  if [ ! -f "$dir/LICENSE" ]; then
    echo "check-licenses: $name is bundled but $dir/LICENSE is missing" >&2
    status=1
  else
    # A file being present is not enough: "A AND B" owes both notices.
    while read -r gap; do
      [ -n "$gap" ] || continue
      if licence_known "$gap"; then
        echo "check-licenses: $name declares \"$licence\" but $dir/ has no $gap text" >&2
      else
        echo "check-licenses: $name declares \"$licence\"; $gap is not a licence scripts/licence-text.sh recognises, so review it before it ships" >&2
      fi
      status=1
    done < <(licence_gaps "$licence" "$dir"/*)
  fi
  # A plain substring: NOTICE groups packages into prose, so it names versions but
  # not always the package id that npm uses.
  if ! grep -qF "$version" NOTICE; then
    echo "check-licenses: NOTICE does not mention $name version $version" >&2
    status=1
  fi
done < <(printf '%s\n' "$bundled")

expected="$(printf '%s\n' "$bundled" | cut -f1 | tr '/' '-' | sort -u)"
# third_party/licenses/go/ holds the Go modules' licences; scripts/go-module-licenses.sh checks it.
present="$(find third_party/licenses -mindepth 1 -maxdepth 1 -type d ! -name go -exec basename {} \; | sort -u)"
while read -r stale; do
  [ -n "$stale" ] || continue
  echo "check-licenses: third_party/licenses/$stale is not bundled any more" >&2
  status=1
done < <(comm -13 <(printf '%s\n' "$expected") <(printf '%s\n' "$present"))

if [ "$status" -eq 0 ]; then
  echo "check-licenses: $(printf '%s\n' "$bundled" | grep -c .) bundled package(s), all attributed"
fi
exit "$status"
