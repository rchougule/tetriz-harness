// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package errgroup wraps golang.org/x/sync/errgroup so a panic inside a spawned
// goroutine becomes an error returned from Wait rather than crashing the process. A
// goroutine does not propagate a panic to its spawner, so recovery must happen inside
// each spawned function; this wrapper does it once for every caller.
package errgroup

import (
	"fmt"
	"runtime/debug"

	"golang.org/x/sync/errgroup"
)

// Group runs functions concurrently and collects the first error (or recovered panic).
type Group interface {
	// Go runs f in a new goroutine; a panic in f is recovered and returned as an error from Wait.
	Go(f func() error)
	// Wait blocks until every Go'd function returns, yielding the first non-nil error.
	Wait() error
}

type group struct {
	*errgroup.Group
}

// New returns a Group with no concurrency limit. Call SetLimit on the embedded group to bound it.
func New() *group {
	return &group{Group: &errgroup.Group{}}
}

// Go runs f in a new goroutine, recovering any panic into a returned error.
func (g *group) Go(f func() error) {
	g.Group.Go(func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic in errgroup: %v \n%s", r, string(debug.Stack()))
			}
		}()
		return f()
	})
}
