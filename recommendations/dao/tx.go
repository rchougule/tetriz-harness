// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"

	"gorm.io/gorm"
)

// txContextKey carries an open *gorm.DB transaction on a context. The terminal-write path threads its
// transaction through the context so the compare-and-set status write and the artifact/finding persist
// run in ONE transaction (see Transactor): a method that reads the transaction with connFromContext
// participates in it, while a method called with a plain context opens its own.
type txContextKey struct{}

// WithTx returns a context carrying tx. A Transactor implementation sets this before invoking the
// function it runs in the transaction, so every DAO call inside that function joins the same one.
func WithTx(ctx context.Context, tx *gorm.DB) context.Context {
	return context.WithValue(ctx, txContextKey{}, tx)
}

// connFromContext returns the transaction carried on ctx when present, else fallback. Both are returned
// with ctx applied, so WithContext cancellation and query hints propagate either way.
func connFromContext(ctx context.Context, fallback *gorm.DB) *gorm.DB {
	if tx, ok := ctx.Value(txContextKey{}).(*gorm.DB); ok && tx != nil {
		return tx.WithContext(ctx)
	}
	return fallback.WithContext(ctx)
}
