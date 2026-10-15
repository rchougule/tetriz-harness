// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/logger"
)

// RegisterMetrics registers every Prometheus metric the engine exports on reg. It is the
// package's single opt-in: importing the package has no side effect (no metric is registered
// at init), so a host calls this once at startup with its own registerer — for example
// prometheus.DefaultRegisterer. Calling it more than once on the same registerer panics, the
// standard MustRegister contract.
func RegisterMetrics(reg prometheus.Registerer) {
	registerEngineMetrics(reg)
	registerValidatorMetrics(reg)
	registerAggregatorMetrics(reg)
	registerBehavioralMetrics(reg)
	registerHarnessImproveMetrics(reg)
	registerHarnessImproveFindingMetrics(reg)
}

// SetLogger installs the zap logger the engine routes every log line through. The default is a
// no-op, so importing the package logs nothing until a host wires a real logger here once at
// startup. Passing nil restores the no-op logger.
func SetLogger(l *zap.Logger) {
	logger.SetLogger(l)
}
