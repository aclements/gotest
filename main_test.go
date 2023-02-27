// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestBasic(t *testing.T) {
	out, err := runTestProg("testprog", "-run=^TestBasic")
	wantExit(t, err, 1)
	wantOutput(t, out, `gathering tests...
testprog:[TestBasicPass]
testprog:[]
testprog:[TestBasicFail]
--- fail TestBasicFail
    main_test.go:14: fail
testprog:[]
=== fail github.com/aclements/gotest/testdata/testprog
`)
}

func TestBuildError(t *testing.T) {
	out, err := runTestProg("builderror")
	wantExit(t, err, 1)
	wantOutput(t, out, `gathering tests...
error: # github.com/aclements/gotest/testdata/builderror [github.com/aclements/gotest/testdata/builderror.test]
error: testdata/builderror/main_test.go:6:2: undefined: x
=== fail github.com/aclements/gotest/testdata/builderror
`)
}

type testRenderer struct {
	s   *state
	buf bytes.Buffer
}

func (r *testRenderer) Status(msg string) (cleanup func()) {
	fmt.Fprintf(&r.buf, "%s\n", msg)
	return func() {}
}
func (r *testRenderer) Start(s *state) { r.s = s }
func (r *testRenderer) Stop()          {}

func (r *testRenderer) Update() {
	any := false
	for i, pkg := range r.s.runningPkgs.o {
		any = true
		if i > 0 {
			r.buf.WriteByte(' ')
		}
		name := strings.TrimPrefix(pkg.name, "github.com/aclements/gotest/testdata/")
		fmt.Fprintf(&r.buf, "%s:[%s]", name, strings.Join(sliceMap(r.s.runningTests[pkg].o, func(t *test) string { return t.name }), " "))
	}
	if any {
		fmt.Fprintf(&r.buf, "\n")
	}
}

func (r *testRenderer) PackageDone(pkg *pkg, action string, output string) {
	fmt.Fprintf(&r.buf, "=== %s %s\n%s", action, pkg.name, pkg.output.String())
}

func (r *testRenderer) TestDone(test *test, action string, output string) {
	fmt.Fprintf(&r.buf, "--- %s %s\n%s", action, test.name, test.output.String())
}

func (r *testRenderer) Error(output string) {
	fmt.Fprintf(&r.buf, "error: %s\n", output)
}

func sliceMap[T, U any](s []T, fn func(T) U) []U {
	out := make([]U, len(s))
	for i := range s {
		out[i] = fn(s[i])
	}
	return out
}

// runTestProgRaw runs testprog and returns the raw terminal output and exit
// status.
func runTestProgRaw(args ...string) (string, error) {
	var out bytes.Buffer
	term := &Term{w: &out}
	err := main1(NewTermRenderer(term), append(args, "./testdata/testprog"))
	return out.String(), err
}

// runTestProg runs testdata/<pkg> and returns the testRenderer output and exit
// status.
func runTestProg(pkg string, args ...string) (string, error) {
	r := new(testRenderer)
	err := main1(r, append(args, "./testdata/"+pkg))
	return r.buf.String(), err
}

func wantExit(t *testing.T, err error, status int) {
	t.Helper()
	if err == nil && status == 0 {
		return
	}
	switch err := err.(type) {
	case ErrExit:
		if int(err) != status {
			t.Errorf("want exit status %d, got %d", status, int(err))
		}
	default:
		t.Errorf("want exit status %d, got error %s", status, err)
	}
}

func wantOutput(t *testing.T, got, want string) {
	t.Helper()
	if got == want {
		return
	}
	t.Errorf("want:\n%sgot:\n%s", want, got)
}
