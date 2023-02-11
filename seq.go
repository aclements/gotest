// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

type Seq[V comparable] struct {
	// TODO: Deletion is O(n) in this. Use a better data structure.
	m map[V]struct{}
	o []V
}

func (s *Seq[V]) Append(v V) (added bool) {
	if s.m == nil {
		s.m = make(map[V]struct{})
	}
	added = true
	if s.Has(v) {
		s.Delete(v)
		added = false
	}
	s.m[v] = struct{}{}
	s.o = append(s.o, v)
	return
}

func (s *Seq[V]) Delete(v V) (present bool) {
	if !s.Has(v) {
		return false
	}
	for i := range s.o {
		if s.o[i] == v {
			copy(s.o[i:], s.o[i+1:])
			s.o = s.o[:len(s.o)-1]
			delete(s.m, v)
			break
		}
	}
	return true
}

func (s *Seq[V]) Has(v V) bool {
	_, ok := s.m[v]
	return ok
}

func (s *Seq[V]) Len() int {
	return len(s.o)
}
