package blob

import (
	"context"
	"errors"
	"testing"
)

func TestFS(t *testing.T) {
	ctx := context.Background()
	s, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("hello")
	h := Hash(body)
	tests := []struct {
		name string
		run  func() error
		want error
	}{
		{"get missing", func() error { _, err := s.Get(ctx, h); return err }, ErrNotFound},
		{"traversal key", func() error { _, err := s.Get(ctx, "../../etc/passwd"); return err }, ErrInvalidHash},
		{"mismatched body", func() error { return s.Put(ctx, h, []byte("other")) }, ErrHashMismatch},
		{"put", func() error { return s.Put(ctx, h, body) }, nil},
		{"put again", func() error { return s.Put(ctx, h, body) }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.run(); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	got, err := s.Get(ctx, h)
	if err != nil || string(got) != "hello" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if ok, err := s.Has(ctx, h); err != nil || !ok {
		t.Fatalf("Has = %v, %v", ok, err)
	}
}
