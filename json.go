// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"time"
)

func jsonRun(args ...string) (*jsonRunner, error) {
	goArgs := append([]string{"test", "-json"}, args...)
	cmd := exec.Command("go", goArgs...)
	cmd.WaitDelay = 5 * time.Second

	p, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe to %s: %w", cmd, err)
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s failed: %w", cmd, err)
	}

	// We do our own line reading instead of using
	// json.Decoder.More because this allows us to recover
	// if there's an error.
	scanner := bufio.NewScanner(p)
	// Set a large line length limit.
	scanner.Buffer(nil, 16<<20)
	return &jsonRunner{cmd: cmd, sc: scanner}, nil
}

type jsonRunner struct {
	cmd *exec.Cmd
	sc  *bufio.Scanner

	queued []testEvent
}

type testEvent struct {
	Time    time.Time // encodes as an RFC3339-format string
	Action  string
	Package string
	Test    string
	Elapsed float64 // seconds
	Output  string
}

var textFailRe = regexp.MustCompile(`^FAIL\t([^ ]+) \[([^]]+)\]$`)

func (r *jsonRunner) next() (testEvent, error) {
	if len(r.queued) == 0 {
		if !r.sc.Scan() {
			err := r.sc.Err()
			if err == nil {
				err = io.EOF
			}
			return testEvent{}, err
		}

		// Now parse what we read from stdout into events.
		var ev testEvent
		err := json.Unmarshal(r.sc.Bytes(), &ev)
		if err == nil {
			r.queued = append(r.queued, ev)
		} else {
			// Up to Go 1.20 at least, build errors are reported in plain text on stderr
			// followed by a plain text FAIL line on stdout. Convert the FAIL line to a
			// testEvent. There's not much we can do for stderr because we don't know
			// what package it was attached to. (Workaround for go.dev/issue/35169 and
			// go.dev/issue/23037)
			if subs := textFailRe.FindSubmatch(r.sc.Bytes()); subs != nil {
				pkg := string(subs[1])
				r.queued = append(r.queued,
					testEvent{Time: time.Now(), Action: "output", Package: pkg, Output: r.sc.Text()},
					testEvent{Time: time.Now(), Action: "fail", Package: pkg},
				)
			} else {
				// Most likely this output came from stderr.
				r.queued = append(r.queued, testEvent{
					Action: "error",
					Output: r.sc.Text(),
				})
			}
		}
	}

	e := r.queued[0]
	r.queued = r.queued[1:]
	return e, nil
}

func (r *jsonRunner) wait() error {
	if r == nil {
		return nil
	}
	return r.cmd.Wait()
}
