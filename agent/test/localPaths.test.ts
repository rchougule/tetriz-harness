import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { scrubLocalPaths } from "../src/engine/redact.js";

describe("scrubLocalPaths — no local layout in a reported reason", () => {
  const roots = [
    { prefix: "/home/dev", replacement: "~" },
    { prefix: "/home/dev/src/widgets", replacement: "acme/widgets" },
  ];

  it("a path under a checkout is relative to its repository, even when the checkout is under home", () => {
    assert.equal(
      scrubLocalPaths("write failed: EACCES: permission denied, open '/home/dev/src/widgets/.claude/rules/a.md.tmp-1-2'", roots),
      "write failed: EACCES: permission denied, open 'acme/widgets/.claude/rules/a.md.tmp-1-2'",
    );
  });

  it("a path under home is ~/…, and home itself is ~", () => {
    assert.equal(scrubLocalPaths("mkdir '/home/dev/.claude/skills/x'", roots), "mkdir '~/.claude/skills/x'");
    assert.equal(scrubLocalPaths("cwd /home/dev", roots), "cwd ~");
  });

  it("anything else absolute is reduced to its basename", () => {
    assert.equal(scrubLocalPaths("ENOENT: no such file, open '/etc/some dir/conf.json'", roots), "ENOENT: no such file, open 'conf.json'");
    assert.equal(scrubLocalPaths("rename /tmp/a/b.tmp -> /var/x/b", roots), "rename b.tmp -> b");
  });

  it("a root is matched only on a path-segment boundary", () => {
    assert.equal(scrubLocalPaths("open '/home/developer/x.md'", roots), "open 'x.md'");
    assert.equal(scrubLocalPaths("open '/home/dev/src/widgets2/x.md'", roots), "open '~/src/widgets2/x.md'");
  });

  it("text with no absolute path is unchanged", () => {
    for (const s of ["download failed: artifact body could not be fetched", "left in place: modified since installed: .claude/CLAUDE.md", "hook activation: settings.json is not valid JSON", "a/b ~/c"]) {
      assert.equal(scrubLocalPaths(s, roots), s);
    }
  });
});
