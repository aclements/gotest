// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"os"
)

type Term struct {
	bytes.Buffer
}

type TermAttr int

var (
	AttrRed    TermAttr = 31
	AttrGreen  TermAttr = 32
	AttrYellow TermAttr = 33
)

func NewTerm() *Term {
	return &Term{}
}

func (t *Term) CursorUp(n int) {
	if n > 0 {
		fmt.Fprintf(t, "\033[%dA", n)
	}
}

func (t *Term) BeginningOfLine() {
	t.WriteByte('\r')
}

// ClearDown clears from the cursor to the end of the screen.
func (t *Term) ClearDown() {
	t.WriteString("\033[J")
}

// ClearRight clears from the cursor to the end of the line.
func (t *Term) ClearRight() {
	t.WriteString("\033[K")
}

// ClearLeft clears from the cursor to the end of the line.
func (t *Term) ClearLeft() {
	t.WriteString("\033[1K")
}

func (t *Term) SetWrap(enable bool) {
	if enable {
		t.WriteString("\033[?7h")
	} else {
		t.WriteString("\033[?7l")
	}
}

func (t *Term) Attr(attrs ...TermAttr) {
	t.WriteString("\033[0")
	for _, attr := range attrs {
		fmt.Fprintf(t, ";%d", attr)
	}
	t.WriteByte('m')
}

func (t *Term) Flush() {
	os.Stdout.Write(t.Bytes())
	t.Reset()
}
