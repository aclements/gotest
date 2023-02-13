// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"net"
	"os"
	"testing"
)

func TestSignalPassthru(t *testing.T) {
	// Let the test running know we're running.
	sockPath := os.Getenv("TEST_SIGNAL_PASSTHRU")
	addr, err := net.ResolveUnixAddr("unix", sockPath)
	if err != nil {
		t.Fatal("resolving address: ", err)
	}
	_, err = net.DialUnix("unix", nil, addr)
	if err != nil {
		t.Fatal("connecting: ", err)
	}
	for {
	}
}
