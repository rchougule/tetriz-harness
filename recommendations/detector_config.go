// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import "github.com/tetriz-ai/tetriz-harness/recommendations/internal/config"

// DetectorConfig tunes the DETECTED detectors: the per-intent-class behavioral bands, the
// invoked-artifact name aliases, the classifier confidence floor, the trailing behavioral window, the
// per-run caps, and the Conflict lenses' markers. Pass it to NewHarnessImproveEngine, or set it on
// ondisk.Options. Each field left at its zero value falls back to the built-in default on its own, so
// a caller sets only the fields it tunes. Start from DefaultDetectorConfig.
type DetectorConfig = config.DetectorConfig

// BehavioralBand is one intent class's acceptable range per behavioral metric. A nil range means the
// metric is not scored for that class.
type BehavioralBand = config.BehavioralBand

// MetricRange is an inclusive [Min, Max] band for one behavioral metric.
type MetricRange = config.MetricRange

// DefaultDetectorConfig returns a DetectorConfig holding the built-in Conflict markers, with every other
// field unset so it resolves to the built-in default. Override fields on the returned value before
// passing it to NewHarnessImproveEngine or ondisk.Options.
func DefaultDetectorConfig() DetectorConfig {
	return config.DefaultDetectorConfig()
}
