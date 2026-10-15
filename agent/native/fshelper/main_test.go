//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func fixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	for _, p := range []string{root, outside, filepath.Join(root, "managed")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{filepath.Join(root, "managed", "file"), filepath.Join(outside, "file")} {
		if err := os.WriteFile(p, []byte("sentinel"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, outside
}
func mustOK(t *testing.T, r response) {
	t.Helper()
	if !r.OK {
		t.Fatalf("%+v", r)
	}
}
func assertSentinel(t *testing.T, outside string) {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(outside, "file"))
	if e != nil || string(b) != "sentinel" {
		t.Fatalf("outside changed: %q %v", b, e)
	}
}
func swap(t *testing.T, root, outside string) {
	t.Helper()
	if err := os.Rename(filepath.Join(root, "managed"), filepath.Join(root, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "managed")); err != nil {
		t.Fatal(err)
	}
}

func TestNormalOperations(t *testing.T) {
	root, _ := fixture(t)
	q := request{Root: root, Path: "new/deep/file", Operation: "write", Data: []byte("hello"), Executable: true}
	r := execute(q, nil)
	mustOK(t, r)
	if r.Created == nil || *r.Created != "new" {
		t.Fatalf("created=%v", r.Created)
	}
	info, e := os.Stat(filepath.Join(root, q.Path))
	if e != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("mode: %v %v", info, e)
	}
	q.Operation = "read"
	r = execute(q, nil)
	mustOK(t, r)
	if string(r.Data) != "hello" {
		t.Fatal(string(r.Data))
	}
	q.Operation = "stat"
	r = execute(q, nil)
	mustOK(t, r)
	if !r.Exists {
		t.Fatal("not occupied")
	}
	q.Operation = "remove"
	r = execute(q, nil)
	mustOK(t, r)
	if !r.Removed {
		t.Fatal("not removed")
	}
	q.Operation = "rmdir"
	q.Path = "new/deep"
	mustOK(t, execute(q, nil))
}

func TestMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing", "root")
	for _, op := range []string{"read", "stat", "remove", "rmdir"} {
		r := execute(request{Operation: op, Root: root, Path: "file"}, nil)
		if r.OK || r.Code != "ENOENT" {
			t.Fatalf("%s: %+v", op, r)
		}
		if _, e := os.Stat(root); !os.IsNotExist(e) {
			t.Fatalf("%s created root", op)
		}
	}
	r := execute(request{Operation: "write", Root: root, Path: "dir/file", Data: []byte("new")}, nil)
	mustOK(t, r)
	if r.Created == nil || *r.Created != "." {
		t.Fatalf("created=%v", r.Created)
	}
}

func TestRejectUncleanPaths(t *testing.T) {
	root, _ := fixture(t)
	for _, p := range []string{"", ".", "..", "../outside/file", "/file", "a/../file", "a//file", "a/./file", "a\\file", "a\x00file"} {
		r := execute(request{Operation: "write", Root: root, Path: p}, nil)
		if r.OK || r.Code != "EINVAL" {
			t.Fatalf("%q: %+v", p, r)
		}
	}
}

func TestSymlinkParentsAndRaces(t *testing.T) {
	for _, op := range []string{"read", "stat", "write", "append", "remove", "rmdir"} {
		for _, race := range []bool{false, true} {
			name := op + "/static"
			if race {
				name = op + "/swap"
			}
			t.Run(name, func(t *testing.T) {
				root, outside := fixture(t)
				path := "managed/file"
				if op == "rmdir" {
					path = "managed/empty"
					for _, p := range []string{filepath.Join(root, path), filepath.Join(outside, "empty")} {
						if e := os.Mkdir(p, 0o755); e != nil {
							t.Fatal(e)
						}
					}
				}
				var h checkpoint
				if race {
					h = func(phase string) {
						if phase == "root-open" {
							swap(t, root, outside)
						}
					}
				} else {
					swap(t, root, outside)
				}
				r := execute(request{Operation: op, Root: root, Path: path, Data: []byte("attack")}, h)
				if r.OK {
					t.Fatalf("escaped operation accepted: %+v", r)
				}
				assertSentinel(t, outside)
				if op == "rmdir" {
					if _, e := os.Stat(filepath.Join(outside, "empty")); e != nil {
						t.Fatal("outside directory removed", e)
					}
				}
			})
		}
	}
}

func TestPinnedWriteParent(t *testing.T) {
	for _, phase := range []string{"parent-open", "temp-open", "before-rename"} {
		t.Run(phase, func(t *testing.T) {
			root, outside := fixture(t)
			r := execute(request{Operation: "write", Root: root, Path: "managed/file", Data: []byte("safe"), Executable: true}, func(p string) {
				if p == phase {
					swap(t, root, outside)
				}
			})
			mustOK(t, r)
			assertSentinel(t, outside)
			b, e := os.ReadFile(filepath.Join(root, "original", "file"))
			if e != nil || string(b) != "safe" {
				t.Fatalf("anchored write missing: %q %v", b, e)
			}
		})
	}
}

func TestPinnedRemoveParent(t *testing.T) {
	for _, op := range []string{"remove", "rmdir"} {
		for _, phase := range []string{"parent-open", "before-remove"} {
			t.Run(op+phase, func(t *testing.T) {
				root, outside := fixture(t)
				path := "managed/file"
				if op == "rmdir" {
					path = "managed/empty"
					for _, p := range []string{filepath.Join(root, path), filepath.Join(outside, "empty")} {
						if e := os.Mkdir(p, 0o755); e != nil {
							t.Fatal(e)
						}
					}
				}
				r := execute(request{Operation: op, Root: root, Path: path}, func(p string) {
					if p == phase {
						swap(t, root, outside)
					}
				})
				mustOK(t, r)
				assertSentinel(t, outside)
				if op == "rmdir" {
					if _, e := os.Stat(filepath.Join(outside, "empty")); e != nil {
						t.Fatal(e)
					}
				}
			})
		}
	}
}

func TestDeletionTypes(t *testing.T) {
	root, outside := fixture(t)
	dir := filepath.Join(root, "empty")
	if e := os.Mkdir(dir, 0o755); e != nil {
		t.Fatal(e)
	}
	if r := execute(request{Operation: "remove", Root: root, Path: "empty"}, nil); r.OK {
		t.Fatal("removed directory")
	}
	if r := execute(request{Operation: "rmdir", Root: root, Path: "managed/file"}, nil); r.OK {
		t.Fatal("removed file")
	}
	if e := os.Symlink(filepath.Join(outside, "file"), filepath.Join(root, "link")); e != nil {
		t.Fatal(e)
	}
	mustOK(t, execute(request{Operation: "remove", Root: root, Path: "link"}, nil))
	assertSentinel(t, outside)
}

func TestInternalRelativeAndAbsoluteSymlinks(t *testing.T) {
	root, _ := fixture(t)
	if e := os.Symlink("managed", filepath.Join(root, "relative")); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(filepath.Join(root, "managed"), filepath.Join(root, "absolute")); e != nil {
		t.Fatal(e)
	}
	for _, op := range []string{"read", "write"} {
		mustOK(t, execute(request{Operation: op, Root: root, Path: "relative/file", Data: []byte("ok")}, nil))
		if r := execute(request{Operation: op, Root: root, Path: "absolute/file"}, nil); r.OK {
			t.Fatalf("accepted absolute link: %s", op)
		}
	}
}

func TestReadRejectsNonregularAndOversize(t *testing.T) {
	root, _ := fixture(t)
	if r := execute(request{Operation: "read", Root: root, Path: "managed"}, nil); r.OK {
		t.Fatal("read directory")
	}
	f, e := os.Create(filepath.Join(root, "large"))
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Truncate(maxRead + 1); e != nil {
		t.Fatal(e)
	}
	_ = f.Close()
	if r := execute(request{Operation: "read", Root: root, Path: "large"}, nil); r.OK {
		t.Fatal("read oversized file")
	}
}

func TestFailedWriteCleansTemporaryFile(t *testing.T) {
	root, _ := fixture(t)
	if e := os.Mkdir(filepath.Join(root, "managed", "dir"), 0o755); e != nil {
		t.Fatal(e)
	}
	if r := execute(request{Operation: "write", Root: root, Path: "managed/dir", Data: []byte("bad")}, nil); r.OK {
		t.Fatal("replaced directory")
	}
	entries, e := os.ReadDir(filepath.Join(root, "managed"))
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".harness-tmp-") {
			t.Fatal("temporary file leaked")
		}
	}
}

func TestBase64Protocol(t *testing.T) {
	root, _ := fixture(t)
	var q request
	raw := fmtRequest(root)
	if e := json.Unmarshal(raw, &q); e != nil {
		t.Fatal(e)
	}
	r := execute(q, nil)
	mustOK(t, r)
	b, e := os.ReadFile(filepath.Join(root, "binary"))
	if e != nil || string(b) != string([]byte{0, 255, 1}) {
		t.Fatalf("%v %v", b, e)
	}
}
func fmtRequest(root string) []byte {
	b, _ := json.Marshal(map[string]any{"operation": "write", "root": root, "path": "binary", "data": "AP8B"})
	return b
}

func TestLeafTypeSwapAtRemoval(t *testing.T) {
	for _, op := range []string{"remove", "rmdir"} {
		t.Run(op, func(t *testing.T) {
			root, outside := fixture(t)
			name := "managed/file"
			if op == "rmdir" {
				name = "managed/empty"
				if e := os.Mkdir(filepath.Join(root, name), 0o755); e != nil {
					t.Fatal(e)
				}
			}
			r := execute(request{Operation: op, Root: root, Path: name}, func(phase string) {
				if phase != "before-remove" {
					return
				}
				if e := os.Rename(filepath.Join(root, name), filepath.Join(root, "saved")); e != nil {
					t.Fatal(e)
				}
				if op == "remove" {
					if e := os.Mkdir(filepath.Join(root, name), 0o755); e != nil {
						t.Fatal(e)
					}
				} else {
					if e := os.Symlink(outside, filepath.Join(root, name)); e != nil {
						t.Fatal(e)
					}
				}
			})
			if r.OK {
				t.Fatal("deleted substituted wrong type")
			}
			assertSentinel(t, outside)
			if _, e := os.Lstat(filepath.Join(root, name)); e != nil {
				t.Fatal("substitute removed", e)
			}
		})
	}
}

func TestDanglingLoopAndFIFO(t *testing.T) {
	root, _ := fixture(t)
	for name, target := range map[string]string{"dangling": "missing", "loop": "loop"} {
		if e := os.Symlink(target, filepath.Join(root, name)); e != nil {
			t.Fatal(e)
		}
		mustOK(t, execute(request{Operation: "stat", Root: root, Path: name}, nil))
		if r := execute(request{Operation: "read", Root: root, Path: name}, nil); r.OK {
			t.Fatalf("read %s", name)
		}
	}
	if e := syscall.Mkfifo(filepath.Join(root, "fifo"), 0o600); e != nil {
		t.Fatal(e)
	}
	if r := execute(request{Operation: "read", Root: root, Path: "fifo"}, nil); r.OK {
		t.Fatal("read FIFO")
	}
}

func TestAppendPreservesConcurrentEntries(t *testing.T) {
	root, outside := fixture(t)
	q := request{Operation: "append", Root: root, Path: "managed/file", Data: []byte("\nagent")}
	r := execute(q, func(phase string) {
		if phase != "append-open" {
			return
		}
		f, e := os.OpenFile(filepath.Join(root, q.Path), os.O_WRONLY|os.O_APPEND, 0)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.WriteString("\nengineer"); e != nil {
			t.Fatal(e)
		}
		if e = f.Close(); e != nil {
			t.Fatal(e)
		}
		swap(t, root, outside)
	})
	mustOK(t, r)
	assertSentinel(t, outside)
	b, e := os.ReadFile(filepath.Join(root, "original", "file"))
	if e != nil || string(b) != "sentinel\nengineer\nagent" {
		t.Fatalf("%q %v", b, e)
	}
}

func TestAppendRejectsOutsideLeaf(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprint(race), func(t *testing.T) {
			root, outside := fixture(t)
			plant := func() {
				if e := os.Remove(filepath.Join(root, "managed", "file")); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(filepath.Join(outside, "file"), filepath.Join(root, "managed", "file")); e != nil {
					t.Fatal(e)
				}
			}
			var h checkpoint
			if race {
				h = func(phase string) {
					if phase == "before-append" {
						plant()
					}
				}
			} else {
				plant()
			}
			r := execute(request{Operation: "append", Root: root, Path: "managed/file", Data: []byte("bad")}, h)
			if r.OK {
				t.Fatal("followed external leaf")
			}
			assertSentinel(t, outside)
		})
	}
}

func TestRemovalRestoresWorkingDirectory(t *testing.T) {
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, _ := fixture(t)
	for _, q := range []request{
		{Operation: "remove", Root: root, Path: "managed/file"},
		{Operation: "remove", Root: root, Path: "managed/missing"},
		{Operation: "rmdir", Root: root, Path: "managed"},
		{Operation: "rmdir", Root: root, Path: "managed/missing"},
	} {
		_ = execute(q, nil)
		after, e := os.Getwd()
		if e != nil || after != before {
			t.Fatalf("%s cwd=%q want %q: %v", q.Operation, after, before, e)
		}
	}
}
