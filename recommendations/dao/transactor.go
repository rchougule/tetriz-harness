// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"

	"gorm.io/gorm"
)

// GormTransactor implements Transactor over a *gorm.DB. It opens one transaction, carries it on the
// context via WithTx, and runs fn against that context so every DAO call inside fn (which reads the
// transaction with connFromContext) joins the same transaction — committing on nil, rolling back on
// error. This is what makes the engine's terminal write atomic: MarkTerminalIfRunning and the
// artifact/finding persist run in ONE transaction, so a persist failure rolls back the status claim.
type GormTransactor struct {
	db *gorm.DB
}

// NewGormTransactor builds a Transactor over db.
func NewGormTransactor(db *gorm.DB) *GormTransactor {
	return &GormTransactor{db: db}
}

var _ Transactor = (*GormTransactor)(nil)

// InTx runs fn inside one transaction. When ctx already carries a transaction (a nested InTx), the
// inner call joins it via a SAVEPOINT, so fn's error still rolls the inner work back without aborting
// the outer transaction.
func (t *GormTransactor) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return connFromContext(ctx, t.db).Transaction(func(tx *gorm.DB) error {
		return fn(WithTx(ctx, tx))
	})
}
