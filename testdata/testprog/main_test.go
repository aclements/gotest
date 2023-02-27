// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import "testing"

func TestBasicPass(t *testing.T) {
	// Passes
}

func TestBasicFail(t *testing.T) {
	t.Fatal("fail")
}
