// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

type Renderer interface {
	// Status renders a progress status message. The caller should call cleanup
	// when it's done.
	Status(msg string) (cleanup func())
	// Start starts rendering state. This can render state at any moment until
	// Stop is called. Nothing else should write to the output surface between
	// Start and Stop.
	Start(s *state)
	// Stop stops rendering state and returns once all rendering is done.
	Stop()
	// Update forces an immediate re-render. Call this when the state has
	// changed in a meaningful way.
	Update()
	// PackageDone displays the results of a completed package.
	PackageDone(pkg *pkg, action string, output string)
	// TestDone displays the results of a complete test.
	TestDone(test *test, action string, output string)
	// Error prints an error.
	Error(output string)
}

type termRenderer struct {
	term    *Term
	updateC chan struct{}
	wg      sync.WaitGroup

	// Callbacks to flush output on the next render
	output []func()

	prevLines int // Lines printed on the last render
}

func NewTermRenderer(term *Term) *termRenderer {
	return &termRenderer{
		term: term,
	}
}

func (r *termRenderer) Status(msg string) (cleanup func()) {
	fmt.Fprintf(r.term, "gathering tests...")
	r.term.Flush()
	return func() {
		r.term.BeginningOfLine()
		r.term.ClearRight()
		r.term.Flush()
	}
}

func (r *termRenderer) Start(s *state) {
	r.updateC = make(chan struct{})
	r.wg.Add(1)
	go func() {
		// Render thread.
		defer r.wg.Done()
		timer := time.NewTimer(0)
		for {
			select {
			case <-timer.C:
			case _, ok := <-r.updateC:
				if !ok {
					return
				}
				if !timer.Stop() {
					<-timer.C
				}
			}
			next := r.render(s, 5)
			r.term.Flush()
			timer.Reset(time.Until(next))
		}
	}()
}

func (r *termRenderer) Stop() {
	close(r.updateC)
	r.wg.Wait()
}

func (r *termRenderer) Update() {
	select {
	case r.updateC <- struct{}{}:
	default:
	}
}

func (r *termRenderer) PackageDone(pkg *pkg, action string, output string) {
	r.queueOutput(func() {
		label := action
		switch action {
		case "pass":
			r.term.Attr(AttrBold, AttrGreen)
			label = "PASS"
		case "fail":
			r.term.Attr(AttrBold, AttrRed)
			label = "FAIL"
		case "skip":
			r.term.Attr(AttrBold, AttrYellow)
			label = "SKIP"
		}
		fmt.Fprintf(r.term, "=== %s %s", label, pkg.name)
		r.term.Attr()
		fmt.Fprintf(r.term, " %d tests", pkg.allDone)
		if pkg.failed > 0 {
			fmt.Fprintf(r.term, ", %d failed", pkg.failed)
		}
		if pkg.skipped > 0 {
			fmt.Fprintf(r.term, ", %d skipped", pkg.skipped)
		}
		r.term.WriteByte('\n')
		if len(output) > 0 {
			r.term.WriteString("    " + strings.ReplaceAll(output, "\n", "\n    ") + "\n")
		}
	})
}

func (r *termRenderer) TestDone(test *test, action string, output string) {
	r.queueOutput(func() {
		label := ""
		switch action {
		case "pass":
			label = "PASS"
			r.term.Attr(AttrGreen)
		case "skip":
			label = "SKIP"
			r.term.Attr(AttrYellow)
		case "fail":
			label = "FAIL"
			r.term.Attr(AttrRed)
		}
		fmt.Fprintf(r.term, "--- %s %s %s\n", label, test.name, test.pkg.name)
		r.term.Attr()
		r.term.WriteString(output)
	})
}

func (r *termRenderer) Error(output string) {
	r.queueOutput(func() {
		r.term.Attr(AttrRed)
		fmt.Fprintf(r.term, "%s\n", output)
		r.term.Attr()
	})
}

func (r *termRenderer) queueOutput(cb func()) {
	r.output = append(r.output, cb)
}

// render draws s to the terminal and returns the time at which render should be
// called again if there are no state changes.
func (r *termRenderer) render(s *state, h int) time.Time {
	s.lock.Lock()
	defer s.lock.Unlock()

	t := r.term

	// TODO: Query actual terminal height and don't exceed it.

	// Clear previous lines
	t.BeginningOfLine()
	t.CursorUp(r.prevLines)
	t.ClearDown()
	r.prevLines = 0

	// Flush output.
	for _, cb := range r.output {
		cb()
	}
	r.output = r.output[:0]

	// Prep for these lines
	t.SetWrap(false)
	defer t.SetWrap(true)

	// Compute the number of packages to show.
	allPkgs := s.runningPkgs.o
	pkgs := allPkgs
	if len(pkgs) > h {
		// Leave one line for "..."
		pkgs = pkgs[:h-1]
	}

	// Compute the package name length to show.
	const pkgNameLimit = 20
	maxPkgName := 0
	for _, pkg := range pkgs {
		if len(pkg.name) > maxPkgName {
			maxPkgName = len(pkg.name)
		}
	}
	if maxPkgName > pkgNameLimit {
		maxPkgName = pkgNameLimit
	}

	next := time.Duration(math.MaxInt64)
	for i, pkg := range pkgs {
		if i > 0 {
			t.WriteString("\n")
			r.prevLines++
		}
		if pkg.failed == 0 {
			t.Attr(AttrGreen)
		} else {
			t.Attr(AttrRed)
		}
		t.WriteString(lTrim(pkg.name, maxPkgName))
		// TODO: Since I only show percentage, not count, maybe it makes sense
		// to add sub-tests as I go, though that would make percent
		// non-monotonic.
		if pkg.mainTests > 0 {
			fmt.Fprintf(t, " %3d%%", int(0.5+100*float64(pkg.done)/float64(pkg.mainTests)))
		}
		t.Attr()
		if pkg.failed > 0 {
			fmt.Fprintf(t, " (%d failed)", pkg.failed)
		}
		// TODO: Print skip count?
		for _, test := range s.runningTests[pkg].o {
			fmt.Fprintf(t, " %s", test.name)
			// TODO: This mixes "test time" and "system time".
			dur := test.extra + time.Since(test.start)
			durStr, next1 := fmtDuration(dur)
			if dur >= time.Second {
				fmt.Fprintf(t, " (%s)", durStr)
			}
			if next1 < next {
				next = next1
			}
		}
	}

	if len(pkgs) < len(allPkgs) {
		fmt.Fprintf(t, "\n... +%d packages ...", len(allPkgs)-len(pkgs))
		r.prevLines++
	}

	return time.Now().Add(next)
}

// lTrim pads s to length or trims the prefix of s to length.
func lTrim(s string, length int) string {
	if len(s) <= length {
		return fmt.Sprintf("%-*s", length, s)
	}
	return "…" + s[len(s)-length+1:]
}

// fmtDuration formats d as a string and returns the duration after d when the
// result will change.
func fmtDuration(d time.Duration) (string, time.Duration) {
	var b strings.Builder
	if d >= time.Hour {
		fmt.Fprintf(&b, "%dh", d/time.Hour)
		d %= time.Hour
	}
	if b.Len() > 0 || d >= time.Minute {
		fmt.Fprintf(&b, "%dm", d/time.Minute)
		d %= time.Minute
	}
	fmt.Fprintf(&b, "%ds", d/time.Second)
	// The output will change when the next second rolls over.
	next := (d + time.Second).Truncate(time.Second) - d
	return b.String(), next
}
