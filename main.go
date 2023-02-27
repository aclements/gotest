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
	r := NewTermRenderer(term)
	err := main1(r, os.Args[1:])
	switch err := err.(type) {
	case nil:
		return
	case ErrExit:
		os.Exit(int(err))
	default:
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
}

type ErrExit int

func (e ErrExit) Error() string {
	return fmt.Sprintf("exit code %d", int(e))
}

func main1(r Renderer, args []string) error {
	// TODO: We often spend a long time "gathering tests" because we're actually
	// recompiling. Maybe I should just start running tests and gather the
	// function names when I see a package start?
	var tests pkgs
	var err error
	func() {
		cleanup := r.Status("gathering tests...")
		defer cleanup()
		tests, err = listTests(args)
	}()
	if err != nil {
		return err
	}

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

func (m pkgs) get(name string) *pkg {
	if p := m[name]; p != nil {
		return p
	}
	p := &pkg{name: name, tests: make(map[string]*test), mainTests: -1}
	m[name] = p
	return p
}

type pkg struct {
	name      string
	mainTests int              // Number of main tests, or -1 if unknown
	tests     map[string]*test // Sub-tests are added while running.

	// Running state
	done    int // Only main tests, but includes failed
	allDone int // Main and sub-tests run, including failed
	failed  int // Includes sub-tests
	skipped int // Includes sub-tests

	output bytes.Buffer
}

func (p *pkg) getTest(name string) *test {
	if t := p.tests[name]; t != nil {
		return t
	}

	mainTestName, _, _ := strings.Cut(name, "/")
	isMain := name == mainTestName
	t := &test{name: name, mainName: mainTestName, pkg: p}
	p.tests[t.name] = t
	if isMain && p.mainTests != -1 {
		p.mainTests++
	}
	return t
}

type test struct {
	name     string
	mainName string
	pkg      *pkg

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
		p := tests.get(ev.Package)
		// The output actions include the final "ok"/"?" line, so we need to
		// filter that out.
		if ev.Action == "output" && ev.Package != "" && !strings.Contains(ev.Output, "\t") {
			testName := strings.TrimRight(ev.Output, "\n")
			p.getTest(testName)
		}
	}
	// Finalize counts for packages we were able to list.
	for _, pkg := range tests {
		if len(pkg.tests) > 0 {
			pkg.mainTests = len(pkg.tests)
		}
	}
	// We ignore failures to list. These will be caught again when trying to run
	// the tests, and those failures will better match user expectations. For
	// example, if we list many packages and one has a build error, the whole
	// list will fail, but we still want to run the other packages.
	out.wait()
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

	if ev.Action == "error" {
		// Unattributed error. Probably something from stderr.
		r.Error(ev.Output)
		return true
	}

	// Package-level logic
	pkg := s.tests.get(ev.Package)
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
	t := pkg.getTest(ev.Test)
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
		if t.name == t.mainName { // Only count main tests toward "done"
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
