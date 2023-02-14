// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"time"
)

func main() {
	setupSignals()
	term := NewTerm()
	err := main1(term, os.Args[1:])
	switch err := err.(type) {
	case nil:
		return
	case ErrExit:
		os.Exit(int(err))
	default:
		// Make sure the terminal is clear
		term.BeginningOfLine()
		term.ClearRight()
		term.Flush()

		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
}

type ErrExit int

func (e ErrExit) Error() string {
	return fmt.Sprintf("exit code %d", int(e))
}

func main1(term *Term, args []string) error {
	// TODO: Test a package with a build error.
	//
	// TODO: We often spend a long time "gathering tests" because we're actually
	// recompiling. Maybe I should just start running tests and gather the
	// function names when I see a package start?
	fmt.Fprintf(term, "gathering tests...")
	term.Flush()
	tests, err := listTests(args)
	if err != nil {
		return err
	}
	term.BeginningOfLine()
	term.ClearRight()
	term.Flush()

	return runTests(term, args, tests)
}

func setupSignals() {
	// Pass through signals to sub-processes.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, signalsToIgnore...)
	go func() {
		<-sig
		// This should kill the subprocess and then this process will exit, but
		// as a backstop, stop ignoring the signals.
		signal.Reset(signalsToIgnore...)
	}()
}

type pkgs map[string]*pkg

type pkg struct {
	name  string
	tests map[string]*test // Sub-tests are added while running.

	// Running state
	mainTests int // Number of main tests
	done      int // Only main tests, but includes failed
	allDone   int // Main and sub-tests run, including failed
	failed    int // Includes sub-tests
	skipped   int // Includes sub-tests

	output bytes.Buffer
}

type test struct {
	name string

	// For running tests.
	start time.Time
	extra time.Duration

	output bytes.Buffer
}

func listTests(args []string) (tests pkgs, err error) {
	// TODO: This is going to be misleading if there's a -test.run argument.
	out, err := jsonRun(append([]string{"-test.list=^Test|^Fuzz|^Example"}, args...)...)
	if err != nil {
		return nil, err
	}
	defer out.cmd.Process.Kill()

	tests = make(pkgs)
	for {
		ev, err := out.next()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("error listing tests: %w", err)
		}
		p := tests[ev.Package]
		if p == nil {
			p = &pkg{name: ev.Package, tests: make(map[string]*test)}
			tests[ev.Package] = p
		}
		// The output actions include the final "ok"/"?" line, so we need to
		// filter that out.
		if ev.Action == "output" && ev.Package != "" && !strings.Contains(ev.Output, "\t") {
			testName := strings.TrimRight(ev.Output, "\n")
			p.tests[testName] = &test{name: testName}
		}
	}
	if err := out.wait(); err != nil {
		return nil, fmt.Errorf("error listing tests: %w", err)
	}
	return tests, nil
}

type state struct {
	lock sync.Mutex

	tests      pkgs
	maxPkgName int

	runningPkgs  Seq[*pkg]
	runningTests map[*pkg]*Seq[*test]

	// Callbacks to flush output on the next render
	output []func(t *Term)

	prevLines int // Lines printed on the last render
}

func runTests(term *Term, args []string, tests pkgs) error {
	maxPkgName := 0
	for _, pkg := range tests {
		// Ignore packages with no tests.
		if len(pkg.tests) > 0 && len(pkg.name) > maxPkgName {
			maxPkgName = len(pkg.name)
		}
		pkg.mainTests = len(pkg.tests)
	}

	out, err := jsonRun(args...)
	if err != nil {
		return err
	}
	defer out.cmd.Process.Kill()

	s := state{
		tests:        tests,
		maxPkgName:   maxPkgName,
		runningTests: make(map[*pkg]*Seq[*test]),
	}
	update := make(chan struct{}, 1)
	go func() {
		// Render thread.
		timer := time.NewTimer(0)
		for {
			select {
			case <-timer.C:
			case _, ok := <-update:
				if !ok {
					break
				}
				if !timer.Stop() {
					<-timer.C
				}
			}
			next := s.render(term, 5)
			term.Flush()
			timer.Reset(time.Until(next))
		}
	}()
	defer close(update)
	for {
		ev, err := out.next()
		if err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("reading from go test: %w", err)
		}

		// TODO: Handle action=="error"

		doUpdate := s.apply(ev)
		if doUpdate || len(s.output) > 0 {
			select {
			case update <- struct{}{}:
			default:
			}
		}
	}

	err = out.wait()
	if err, ok := err.(*exec.ExitError); ok && err.Exited() {
		return ErrExit(err.ExitCode())
	}
	return err
}

func (s *state) queueOutput(cb func(t *Term)) {
	s.output = append(s.output, cb)
}

// apply updates the state with the given testEvent and returns whether the
// state needs to be re-rendered.
func (s *state) apply(ev testEvent) bool {
	s.lock.Lock()
	defer s.lock.Unlock()

	// Package-level logic
	pkg := s.tests[ev.Package]
	if pkg == nil {
		// This shouldn't happen because we started with a package list.
		//
		// TODO: Maybe be more robust to this.
		return false
	}
	if !s.runningPkgs.Has(pkg) {
		s.runningPkgs.Append(pkg)
		if _, ok := s.runningTests[pkg]; !ok {
			s.runningTests[pkg] = new(Seq[*test])
		}
	}
	if ev.Test == "" {
		switch ev.Action {
		case "start":
			// We already started the package, so nothing more to do.
			return false
		case "output":
			// TODO: Test a package that crashes before running any tests.
			pkg.output.WriteString(ev.Output)
			return false
		case "pass", "fail", "skip":
			// Package is done. If the package failed and there are still tests
			// running, flag those tests as failed. This can happen if a test
			// fatally panics, in which case that fatal panic output is
			// attributed to the test, but there's no "FAIL" line.
			//
			// TODO: Test a package that fatally crashes during a test. Make
			// sure the output appears even though there's no FAIL line.
			if ev.Action == "fail" {
				for _, t := range s.runningTests[pkg].o {
					s.applyTest(pkg, testEvent{Action: "fail", Test: t.name})
				}
			}
			s.runningPkgs.Delete(pkg)
			// Process non-test package output. This includes the final status
			// line, which is redundant, so we trim that.
			output := pkg.output.String()
			pkg.output = bytes.Buffer{}
			if strings.HasSuffix(output, "\n") {
				lastLine := 1 + strings.LastIndex(output[:len(output)-1], "\n")
				l := output[lastLine:]
				if strings.HasPrefix(l, "ok  ") || strings.HasPrefix(l, "FAIL") || strings.HasPrefix(l, "?   ") {
					output = output[:lastLine]
				}
			}
			// Also trim the PASS/FAIL line.
			if strings.HasSuffix(output, "PASS\n") || strings.HasSuffix(output, "FAIL\n") {
				output = output[:len(output)-len("PASS\n")]
			}
			if strings.HasSuffix(output, "\n") {
				// We'll add this back when printing.
				output = output[:len(output)-1]
			}
			// Print the package status.
			//
			// TODO: Should we try to keep things in order? It's probably better
			// to report ASAP. If I really wanted to, I could keep the running
			// lines in order and change a running line to an ok line when done
			// and when it reaches the top let it pop out of the redraw region.
			// If I did something like that, I should probably just use almost
			// the whole terminal height. Alternatively, maybe I don't report
			// the package status lines at all and just show a summary line?
			s.queueOutput(func(term *Term) {
				label := ""
				switch ev.Action {
				case "pass":
					term.Attr(AttrBold, AttrGreen)
					label = "PASS"
				case "fail":
					term.Attr(AttrBold, AttrRed)
					label = "FAIL"
				case "skip":
					term.Attr(AttrBold, AttrYellow)
					label = "SKIP"
				}
				fmt.Fprintf(term, "=== %s %s", label, ev.Package)
				term.Attr()
				fmt.Fprintf(term, " %d tests", pkg.allDone)
				if pkg.failed > 0 {
					fmt.Fprintf(term, ", %d failed", pkg.failed)
				}
				if pkg.skipped > 0 {
					fmt.Fprintf(term, ", %d skipped", pkg.skipped)
				}
				term.WriteByte('\n')
				if len(output) > 0 {
					term.WriteString("    " + strings.ReplaceAll(output, "\n", "\n    ") + "\n")
				}
			})
			return true
		}
		return false
	}

	return s.applyTest(pkg, ev)
}

func (s *state) applyTest(pkg *pkg, ev testEvent) bool {
	// Test-level logic
	mainTestName, _, _ := strings.Cut(ev.Test, "/")
	isMain := ev.Test == mainTestName
	t := pkg.tests[ev.Test]
	if t == nil {
		t = &test{name: ev.Test}
		pkg.tests[t.name] = t
		if isMain {
			// Unexpected. Update the counter.
			pkg.mainTests++
		}
	}
	// TODO: What happens if a test times out? What if it times out at the
	// cmd/go level?
	switch ev.Action {
	default:
		// Unknown action
		return false
	case "run", "cont":
		s.runningTests[pkg].Append(t)
		t.start = ev.Time
		return true
	case "pause":
		t.extra = ev.Time.Sub(t.start)
		t.start = time.Time{}
		s.runningTests[pkg].Delete(t)
		return true
	case "output":
		// Strip control messages. These get parsed by test2json.
		if strings.HasPrefix(ev.Output, "=== ") ||
			strings.HasPrefix(ev.Output, "--- ") {
			return false
		}
		// TODO: Test a failed subtest. In text format, these get indented until
		// the parent test and the parent test prints a fail line, but I'm not
		// sure what happens in verbose format.
		//
		// Buffer the test output so we can print it if it fails.
		t.output.WriteString(ev.Output)
		return false
	case "fail":
		// TODO: Do I care about test order? Package order? If it's not in
		// package order, how do I show which package a failed test is in? Maybe
		// I just print custom fail lines anyway so I can put whatever I want.
		//
		// TODO: Print a summary and record the whole log somewhere with a
		// command to print the details of a failed test (or any test).
		output := t.output.Bytes()
		s.queueOutput(func(term *Term) {
			label := ""
			switch ev.Action {
			case "pass":
				label = "PASS"
				term.Attr(AttrGreen)
			case "skip":
				label = "SKIP"
				term.Attr(AttrYellow)
			case "fail":
				label = "FAIL"
				term.Attr(AttrRed)
			}
			fmt.Fprintf(term, "--- %s %s %s\n", label, ev.Test, ev.Package)
			term.Attr()
			term.Write(output)
		})
		fallthrough
	case "pass", "skip":
		s.runningTests[pkg].Delete(t)
		if isMain {
			pkg.done++
		}
		pkg.allDone++
		if ev.Action == "fail" {
			pkg.failed++
		} else if ev.Action == "skip" {
			pkg.skipped++
		}
		// Release test output memory.
		t.output = bytes.Buffer{}
		return true
	}
}

// render draws s to the terminal and returns the time at which render should be
// called again if there are no state changes.
func (s *state) render(t *Term, h int) time.Time {
	s.lock.Lock()
	defer s.lock.Unlock()

	// TODO: Query actual terminal height and don't exceed it.

	// Clear previous lines
	t.BeginningOfLine()
	t.CursorUp(s.prevLines)
	t.ClearDown()
	s.prevLines = 0

	// Flush output.
	for _, cb := range s.output {
		cb(t)
	}
	s.output = s.output[:0]

	// Prep for these lines
	t.SetWrap(false)
	defer t.SetWrap(true)

	// Compute the number of packages to show.
	maxPkgs := s.runningPkgs.Len()
	trimmedPkgs := 0
	if maxPkgs > h {
		trimmedPkgs = maxPkgs - (h - 1)
		maxPkgs = h - 1
	}

	// Compute the package name length to show.
	const pkgNameLimit = 20
	maxPkgName := s.maxPkgName
	if maxPkgName > pkgNameLimit {
		maxPkgName = pkgNameLimit
	}

	next := time.Duration(math.MaxInt64)
	for i, pkg := range s.runningPkgs.o {
		if i > 0 {
			t.WriteString("\n")
			s.prevLines++
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
		fmt.Fprintf(t, " %3d%%", int(0.5+100*float64(pkg.done)/float64(pkg.mainTests)))
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

	if trimmedPkgs > 0 {
		fmt.Fprintf(t, "\n... +%d packages ...", trimmedPkgs)
		s.prevLines++
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
