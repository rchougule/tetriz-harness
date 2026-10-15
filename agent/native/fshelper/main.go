//go:build darwin || linux

// fshelper executes one bounded filesystem operation. The configured root and
// its ancestors are trusted; all descendants may change concurrently. Native
// os.Root operations enforce traversal resistance below that root. Mount-point
// and hard-link attacks are outside this boundary.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const maxRead = 16 << 20

type request struct {
	Operation  string `json:"operation"`
	Root       string `json:"root"`
	Path       string `json:"path"`
	Data       []byte `json:"data"`
	Executable bool   `json:"executable"`
}
type response struct {
	OK      bool    `json:"ok"`
	Data    []byte  `json:"data,omitempty"`
	Created *string `json:"created,omitempty"`
	Exists  bool    `json:"exists,omitempty"`
	Removed bool    `json:"removed,omitempty"`
	Error   string  `json:"error,omitempty"`
	Code    string  `json:"code,omitempty"`
}

// checkpoint is supplied only by in-package tests, never by input or environment.
type checkpoint func(string)

func mark(h checkpoint, phase string) {
	if h != nil {
		h(phase)
	}
}

func main() {
	var q request
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 24<<20))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&q)
	if err == nil {
		var extra any
		if e := decoder.Decode(&extra); e != io.EOF {
			err = fmt.Errorf("expected exactly one request")
		}
	}
	var out response
	if err != nil {
		out = failure(err)
	} else {
		out = execute(q, nil)
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

func failure(err error) response {
	code := "EIO"
	switch {
	case errors.Is(err, fs.ErrNotExist):
		code = "ENOENT"
	case errors.Is(err, fs.ErrPermission):
		code = "EACCES"
	case errors.Is(err, fs.ErrExist):
		code = "EEXIST"
	case errors.Is(err, syscall.ENOTDIR):
		code = "ENOTDIR"
	case errors.Is(err, syscall.EISDIR):
		code = "EISDIR"
	case errors.Is(err, syscall.ENOTEMPTY):
		code = "ENOTEMPTY"
	case errors.Is(err, syscall.EINVAL):
		code = "EINVAL"
	case errors.Is(err, syscall.ELOOP):
		code = "ELOOP"
	}
	return response{Error: err.Error(), Code: code}
}

func validPath(p string) bool {
	return p != "" && p != "." && !filepath.IsAbs(p) && filepath.Clean(p) == p &&
		!strings.ContainsRune(p, '\\') && !strings.ContainsRune(p, 0) &&
		p != ".." && !strings.HasPrefix(p, ".."+string(filepath.Separator))
}

// openRoot creates a missing configured root only for writes. Its ancestor
// lookup is part of the trusted-root boundary, not descendant containment.
func openRoot(name string, create bool) (*os.Root, bool, error) {
	r, err := os.OpenRoot(name)
	if err == nil || !create || !errors.Is(err, fs.ErrNotExist) {
		return r, false, err
	}
	ancestor := filepath.Clean(name)
	for {
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return nil, false, err
		}
		ancestor = parent
		anchor, e := os.OpenRoot(ancestor)
		if errors.Is(e, fs.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, false, e
		}
		rel, e := filepath.Rel(ancestor, name)
		if e == nil {
			e = anchor.MkdirAll(rel, 0o755)
		}
		if e == nil {
			r, e = anchor.OpenRoot(rel)
		}
		_ = anchor.Close()
		return r, e == nil, e
	}
}

func execute(q request, h checkpoint) response {
	if !validPath(q.Path) || q.Root == "" || !filepath.IsAbs(q.Root) {
		return failure(fmt.Errorf("root must be absolute and path a clean nonempty relative name: %w", syscall.EINVAL))
	}
	switch q.Operation {
	case "read", "stat", "write", "append", "remove", "rmdir":
	default:
		return failure(fmt.Errorf("unknown operation: %w", syscall.EINVAL))
	}
	if len(q.Data) > maxRead {
		return failure(fmt.Errorf("content exceeds 16 MiB: %w", syscall.EINVAL))
	}
	root, rootCreated, err := openRoot(q.Root, q.Operation == "write" || q.Operation == "append")
	if err != nil {
		return failure(err)
	}
	defer func() { _ = root.Close() }()
	mark(h, "root-open")
	switch q.Operation {
	case "stat":
		_, err = root.Lstat(q.Path)
		if err != nil {
			return failure(err)
		}
		return response{OK: true, Exists: true}
	case "read":
		mark(h, "before-read")
		// Nonblocking open avoids hanging on an attacker-substituted FIFO before
		// the descriptor's regular-file check. No data is read from other types.
		f, e := root.OpenFile(q.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if e != nil {
			return failure(e)
		}
		defer func() { _ = f.Close() }()
		info, e := f.Stat()
		if e != nil {
			return failure(e)
		}
		if !info.Mode().IsRegular() {
			return failure(fmt.Errorf("read requires a regular file: %w", syscall.EINVAL))
		}
		if info.Size() > maxRead {
			return failure(fmt.Errorf("file exceeds 16 MiB: %w", syscall.EINVAL))
		}
		data, e := io.ReadAll(io.LimitReader(f, maxRead+1))
		if e != nil {
			return failure(e)
		}
		if len(data) > maxRead {
			return failure(fmt.Errorf("file exceeds 16 MiB: %w", syscall.EINVAL))
		}
		return response{OK: true, Data: data}
	case "write":
		return write(root, q, rootCreated, h)
	case "append":
		return appendFile(root, q, rootCreated, h)
	default:
		return remove(root, q.Path, q.Operation == "rmdir", h)
	}
}

func makeParents(root *os.Root, dir string) (*string, error) {
	if dir == "." {
		return nil, nil
	}
	var created *string
	var prefix string
	for _, part := range strings.Split(dir, string(filepath.Separator)) {
		prefix = filepath.Join(prefix, part)
		err := root.Mkdir(prefix, 0o755)
		if err == nil {
			if created == nil {
				p := prefix
				created = &p
			}
			continue
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		info, err := root.Stat(prefix)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, syscall.ENOTDIR
		}
	}
	return created, nil
}

func write(root *os.Root, q request, rootCreated bool, h checkpoint) response {
	created, err := makeParents(root, filepath.Dir(q.Path))
	if err != nil {
		return failure(err)
	}
	if rootCreated {
		p := "."
		created = &p
	}
	mark(h, "before-parent-open")
	parent, err := root.OpenRoot(filepath.Dir(q.Path))
	if err != nil {
		return failure(err)
	}
	defer func() { _ = parent.Close() }()
	mark(h, "parent-open")
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return failure(err)
	}
	tmp := ".harness-tmp-" + hex.EncodeToString(random[:])
	f, err := parent.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return failure(err)
	}
	// Preserve the operation error if cleanup also fails. Successful rename
	// already removed this temporary name.
	defer func() { _ = parent.Remove(tmp) }()
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()
	mark(h, "temp-open")
	if _, err = f.Write(q.Data); err != nil {
		return failure(err)
	}
	if q.Executable {
		if err = f.Chmod(0o755); err != nil {
			return failure(err)
		}
	}
	if err = f.Close(); err != nil {
		return failure(err)
	}
	closed = true
	mark(h, "before-rename")
	if err = parent.Rename(tmp, filepath.Base(q.Path)); err != nil {
		return failure(err)
	}
	return response{OK: true, Created: created}
}

// The helper handles one request per process. Chdir to an opened parent lets
// portable Unix unlink/rmdir syscalls enforce file-only/directory-only deletion
// without a vulnerable lstat-then-os.Remove type check. No pathname ancestor is
// resolved after the directory handle has been acquired.
func remove(root *os.Root, path string, directory bool, h checkpoint) (out response) {
	mark(h, "before-parent-open")
	parent, err := root.OpenRoot(filepath.Dir(path))
	if err != nil {
		return failure(err)
	}
	defer func() { _ = parent.Close() }()
	mark(h, "parent-open")
	dir, err := parent.Open(".")
	if err != nil {
		return failure(err)
	}
	defer func() { _ = dir.Close() }()
	previous, err := os.Open(".")
	if err != nil {
		return failure(err)
	}
	defer func() { _ = previous.Close() }()
	if err = dir.Chdir(); err != nil {
		return failure(err)
	}
	defer func() {
		if err := previous.Chdir(); err != nil && out.OK {
			out = failure(fmt.Errorf("restoring working directory: %w", err))
		}
	}()
	mark(h, "before-remove")
	if directory {
		err = syscall.Rmdir(filepath.Base(path))
	} else {
		err = syscall.Unlink(filepath.Base(path))
	}
	if err != nil {
		return failure(err)
	}
	return response{OK: true, Removed: true}
}

// appendFile retains O_APPEND so independently appended user entries are not
// discarded by a read/modify/rewrite cycle. Root.OpenFile resolves every link
// under the original root, including the leaf.
func appendFile(root *os.Root, q request, rootCreated bool, h checkpoint) response {
	created, err := makeParents(root, filepath.Dir(q.Path))
	if err != nil {
		return failure(err)
	}
	if rootCreated {
		p := "."
		created = &p
	}
	mark(h, "before-append")
	f, err := root.OpenFile(q.Path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NONBLOCK, 0o644)
	if err != nil {
		return failure(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return failure(err)
	}
	if !info.Mode().IsRegular() {
		return failure(fmt.Errorf("append requires a regular file: %w", syscall.EINVAL))
	}
	mark(h, "append-open")
	if _, err = f.Write(q.Data); err != nil {
		return failure(err)
	}
	// Closing an append descriptor can report a delayed write error; do not
	// claim success until both writing and closing have succeeded.
	if err = f.Close(); err != nil {
		return failure(err)
	}
	return response{OK: true, Created: created}
}
