// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

const signalPassthruEnv = "TEST_SIGNAL_PASSTHRU"

func init() {
	// Sub-process part of TestSignalPassthru.
	if os.Getenv(signalPassthruEnv) == "" {
		return
	}
	// runTestProg normally skips signal setup, so do it explicitly.
	setupSignals()
	out, err := runTestProg("-run=TestSignalPassthru")
	// Tests just exit 1 when interrupted, so check for that error.
	switch err := err.(type) {
	case ErrExit:
		if int(err) == 1 {
			fmt.Println("ok")
			os.Exit(0)
		}
	}
	fmt.Println(err)
	fmt.Println(string(out))
	os.Exit(1)
}

func TestSignalPassthru(t *testing.T) {
	// Start gotest as a sub-process that we can signal. It will in turn start
	// the testprog as a sub-process.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tdir := t.TempDir()
	addr, err := net.ResolveUnixAddr("unix", filepath.Join(tdir, "sock"))
	if err != nil {
		t.Fatal("resolving address:", err)
	}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal("listening:", err)
	}
	defer l.Close()
	cmd := exec.Command(exe)
	cmd.Env = append(cmd.Environ(), signalPassthruEnv+"="+addr.Name)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// TODO: If gotest exits without running the test, Accept will block. Wait
	// until either gotest exits or we get a connection.

	// Wait for the test to start
	conn, err := l.Accept()
	if err != nil {
		t.Fatal("accepting:", err)
	}
	conn.Close()

	// Signal the gotest process group.
	syscall.Kill(-cmd.Process.Pid, os.Interrupt.(syscall.Signal))

	// gotest should exit 1.
	if err := cmd.Wait(); err != nil {
		t.Fatalf("go test failed: %s\nOutput:\n%s\n%s", err, outBuf.String(), errBuf.String())
	}
	if outBuf.String() != "ok\n" {
		t.Fatalf("want ok, got:\n%s\n%s", outBuf.String(), errBuf.String())
	}
}

func runTestProg(args ...string) ([]byte, error) {
	var out bytes.Buffer
	term := &Term{w: &out}
	err := main1(term, append(args, "./testdata/testprog"))
	return out.Bytes(), err
}
