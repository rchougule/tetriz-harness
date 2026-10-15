// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package logger is a thin context-aware logging facade over zap, vendored so the
// recommendations engine does not depend on a host application's logging package.
//
// It exposes the small surface the engine uses — Info/Error/WarnWithCtx plus the
// structured field-key constants — backed by a process-global zap logger.
// The global default is a no-op; a host wires a real logger once via SetLogger.
package logger

import (
	"context"
	"sync/atomic"

	"go.uber.org/zap"
)

// Field-key constants keep structured log keys consistent across the engine.
const (
	// ORG_ID is the structured key for the tenant/organization identifier.
	ORG_ID = "org_id"
	// REPO_ID is the structured key for the repository identifier.
	REPO_ID = "repo_id"
)

// current holds the active *zap.Logger. It defaults to a no-op so importing this
// package has no side effect and logging never panics before a host configures it.
var current atomic.Pointer[zap.Logger]

func active() *zap.Logger {
	if l := current.Load(); l != nil {
		return l
	}
	return zap.NewNop()
}

// SetLogger installs the logger every package-level call routes through. Passing nil
// restores the no-op logger. Not safe to call concurrently with heavy logging churn,
// so a host sets it once at startup.
func SetLogger(l *zap.Logger) {
	current.Store(l)
}

// Info logs at info level.
func Info(_ context.Context, msg string, fields ...zap.Field) {
	active().Info(msg, fields...)
}

// Error logs at error level.
func Error(_ context.Context, msg string, fields ...zap.Field) {
	active().Error(msg, fields...)
}

// WarnWithCtx logs at warn level.
func WarnWithCtx(_ context.Context, msg string, fields ...zap.Field) {
	active().Warn(msg, fields...)
}
