// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// A bad import causes a "[setup failed]" message from cmd/go because
// it fails in package graph setup, before it can even get to the
// build.
//
// "[setup failed]" can also occur with various low-level failures in
// cmd/go, like failing to create a temporary directory.

package main

import _ "x"
