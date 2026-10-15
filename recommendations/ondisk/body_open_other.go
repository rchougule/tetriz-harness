//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris

// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package ondisk

// These platforms do not expose Unix FIFOs through ordinary filesystem opens.
const bodyOpenFlags = 0
