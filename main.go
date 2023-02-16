// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"io"
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

	r := NewTermRenderer(term)
	return runTests(r, args, tests)
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
	name      string
	mainTests int              // Number of main tests, or 0 if unknown
	tests     map[string]*test // Sub-tests are added while running.

	// Running state
	done    int // Only main tests, but includes failed
	allDone int // Main and sub-tests run, including failed
	failed  int // Includes sub-tests
	skipped int // Includes sub-tests

	output bytes.Buffer
}

type test struct {
	name string
	pkg  *pkg

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
			p.tests[testName] = &test{name: testName, pkg: p}
		}
	}
	for _, pkg := range tests {
		pkg.mainTests = len(pkg.tests)
	}
	if err := out.wait(); err != nil {
		return nil, fmt.Errorf("error listing tests: %w", err)
	}
	return tests, nil
}

func runTests(r Renderer, args []string, tests pkgs) error {
	out, err := jsonRun(args...)
	if err != nil {
		return err
	}
	defer out.cmd.Process.Kill()

	s := state{
		tests:        tests,
		runningTests: make(map[*pkg]*Seq[*test]),
	}
	r.Start(&s)
	defer r.Stop()
	for {
		ev, err := out.next()
		if err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("reading from go test: %w", err)
		}

		// TODO: Handle action=="error"

		doUpdate := s.apply(r, ev)
		if doUpdate {
			r.Update()
		}
	}

	err = out.wait()
	if err, ok := err.(*exec.ExitError); ok && err.Exited() {
		return ErrExit(err.ExitCode())
	}
	return err
}

// state interprets a stream of testEvents.
type state struct {
	lock sync.Mutex

	tests pkgs

	runningPkgs  Seq[*pkg]
	runningTests map[*pkg]*Seq[*test]
}

// apply updates the state with the given testEvent and returns whether the
// state needs to be re-rendered.
func (s *state) apply(r Renderer, ev testEvent) bool {
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
					s.applyTest(r, pkg, testEvent{Action: "fail", Test: t.name})
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
			r.PackageDone(pkg, ev.Action, output)
			return true
		}
		return false
	}

	return s.applyTest(r, pkg, ev)
}

func (s *state) applyTest(r Renderer, pkg *pkg, ev testEvent) bool {
	// Test-level logic
	mainTestName, _, _ := strings.Cut(ev.Test, "/")
	isMain := ev.Test == mainTestName
	t := pkg.tests[ev.Test]
	if t == nil {
		t = &test{name: ev.Test, pkg: pkg}
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
	case "pass", "fail", "skip":
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
		if ev.Action == "fail" {
			// TODO: Do I care about test order? Package order? If it's not in
			// package order, how do I show which package a failed test is in? Maybe
			// I just print custom fail lines anyway so I can put whatever I want.
			//
			// TODO: Print a summary and record the whole log somewhere with a
			// command to print the details of a failed test (or any test).
			r.TestDone(t, ev.Action, t.output.String())
		}
		// Release test output memory.
		t.output = bytes.Buffer{}
		return true
	}
}
