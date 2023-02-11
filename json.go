// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

func jsonRun(args ...string) (*jsonRunner, error) {
	goArgs := append([]string{"test", "-json"}, args...)
	cmd := exec.Command("go", goArgs...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Stderr = os.Stderr
	p, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe to %s: %w", cmd, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s failed: %w", cmd, err)
	}
	// We do our own line reading instead of using
	// json.Decoder.More because this allows us to recover
	// if there's an error.
	scanner := bufio.NewScanner(p)
	// Set a large line length limit.
	scanner.Buffer(nil, 16<<20)
	return &jsonRunner{cmd, scanner}, nil
}

type jsonRunner struct {
	cmd *exec.Cmd
	sc  *bufio.Scanner
}

type testEvent struct {
	Time    time.Time // encodes as an RFC3339-format string
	Action  string
	Package string
	Test    string
	Elapsed float64 // seconds
	Output  string
}

func (r *jsonRunner) next() (testEvent, error) {
	if !r.sc.Scan() {
		err := r.sc.Err()
		if err == nil {
			err = io.EOF
		}
		return testEvent{}, err
	}
	var ev testEvent
	err := json.Unmarshal(r.sc.Bytes(), &ev)
	if err == nil {
		return ev, nil
	}
	return testEvent{
		Action: "error",
		Output: fmt.Sprintf("non-test2json line: %s", r.sc.Text()),
	}, nil
}

func (r *jsonRunner) wait() error {
	if r == nil {
		return nil
	}
	// Wait will close the stdout pipe
	return r.cmd.Wait()
}
