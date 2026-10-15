// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import "context"

// scopedRead runs fn with ctx and returns its error. Single-tenant: a read needs no wrapping
// transaction, so this simply invokes fn directly. It exists so the read call sites keep their
// closure shape; the terminal-write path uses dao.Transactor.InTx (one transaction) for the
// compare-and-set + persist atomicity guarantee, never this.
func scopedRead(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}
