// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix || js

package main

import (
	"os"
	"syscall"
)

var signalsToIgnore = []os.Signal{syscall.SIGINT, syscall.SIGQUIT}
