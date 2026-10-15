// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package recommendations is the harness recommendation engine. It reads an organization's committed AI
// coding harness and its engineering signals, and produces two kinds of finding:
//
//   - DETECTED — evidence-backed problems in the harness as it stands (duplicate rule ids, dead globs,
//     over-broad permission grants, and the like), found by config-only detectors over the committed
//     files. These need no LLM and run against a local Postgres alone.
//   - DERIVED — net-new artifacts the org's usage calls for (hooks, agents, rules), recommended from
//     session, review, and PR signals. These go through the tetriz-py LLM service; with no service
//     wired the DERIVED legs degrade and only the DETECTED detectors contribute.
//
// Every finding reconciles into the prompt_library table so the latest run's findings are the live
// (is_latest) set, and each run's coverage names the detector families that ran and the ones skipped for
// lack of input, so a zero-finding scan is never read as a clean bill of health.
//
// The engine is a single package: the DAOs, detectors, aggregator, quality gate, and LLM seams all live
// here, persisting to Postgres via GORM. The engine is single-tenant — every record for the organization
// is processed. For the zero-setup entry point — point it at a harness directory on disk and get the
// DETECTED findings with no org/repo ids and no upstream data — use the ondisk subpackage.
package recommendations
