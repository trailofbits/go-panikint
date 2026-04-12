// errorcheck -0 -d=ssa/known_bits/debug=1

//go:build amd64 || arm64 || s390x || ppc64le || riscv64

// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package a

func knownBitsPhiAnd(cond bool) int {
	x := 1
	if cond {
		x = 3
	}
	return x & 1 // ERROR "known value of v[0-9]+ \(And64\): 1$"
}

func knownBitsPhiAndGarbage(cond bool, x int) int {
	x &^= 1
	if cond {
		x = 2
	}
	return x & 1 // ERROR "known value of v[0-9]+ \(And64\): 0$"
}

func unknownBitsPhiAnd(cond bool) int {
	x := 1
	if cond {
		x = 2
	}
	return x & 1
}

func knownBitsOrGarbage(x, unknown int) int {
	x |= 7
	x |= unknown &^ 3
	return x & 3 // ERROR "known value of v[0-9]+ \(And64\): 3$"
}

func unknownBitsOrGarbage(x, unknown int) int {
	x |= 1
	x |= unknown
	return x & 3
}

func knownBitsDeferPattern(a, b bool) int {
	bits := 0
	bits |= 1 << 0
	if a {
		bits |= 1 << 1
	}
	bits |= 1 << 2
	if b {
		bits |= 1 << 3
	}
	return bits & (1<<2 | 1<<0) // ERROR "known value of v[0-9]+ \(And64\): 5$"
}

func knownBitsDeferPatternGarbage(a, b bool, garbage int) int {
	bits := 0
	bits |= 1 << 0
	if a {
		bits |= 1 << 1
	}
	bits |= 1 << 2
	if b {
		bits |= 1 << 3
	}
	bits ^= garbage &^ (1<<2 | 1<<0)
	return bits & (1<<2 | 1<<0) // ERROR "known value of v[0-9]+ \(And64\): 5$"
}

func knownBitsXorToggle(a, b, c bool) int {
	bits := 0
	bits ^= 1 << 0
	if a {
		bits ^= 1 << 1
	}
	bits ^= 1 << 2
	if b {
		bits ^= 1 << 3
	}
	bits ^= 1 << 2
	if c {
		bits ^= 1 << 4
	}
	return bits & (1<<2 | 1<<0) // ERROR "known value of v[0-9]+ \(And64\): 1$"
}

func knownBitsXorToggleGarbage(a, b, c bool, garbage int) int {
	bits := 0
	bits ^= 1 << 0
	if a {
		bits ^= 1 << 1
	}
	bits ^= 1 << 2
	if b {
		bits ^= 1 << 3
	}
	bits ^= 1 << 2
	if c {
		bits ^= 1 << 4
	}
	bits ^= garbage &^ (1<<2 | 1<<0)
	return bits & (1<<2 | 1<<0) // ERROR "known value of v[0-9]+ \(And64\): 1$"
}

func unknownBitsXorToggle(a, b, c bool) int {
	bits := 0
	bits ^= 1 << 0
	if a {
		bits ^= 1 << 1
	}
	bits ^= 1 << 2
	if b {
		bits ^= 1 << 2
	}
	bits ^= 1 << 2
	if c {
		bits ^= 1 << 4
	}
	return bits & (1<<2 | 1<<0)
}

func knownBitsPhiComAnd(cond bool) int {
	x := 1
	if cond {
		x = 3
	}
	return ^x & 1 // ERROR "known value of v[0-9]+ \(And64\): 0$"
}

func knownBitsPhiComAndGarbage(cond bool, garbage int) int {
	x := 1
	if cond {
		x = 3
	}
	x ^= garbage &^ 1
	return ^x & 1 // ERROR "known value of v[0-9]+ \(And64\): 0$"
}

func unknownBitsPhiComAnd(cond bool) int {
	x := 1
	if cond {
		x = 2
	}
	return ^x & 1
}

func knownBitsEqFalse(x, y uint64) bool {
	x |= 1
	y &^= 1
	return x == y // ERROR "known value of v[0-9]+ \(Eq64\): false$"
}

func knownBitsEqTrue(x uint64, cond bool) bool {
	x |= (1<<32 - 1) << 32
	if cond {
		x |= 42
	}
	x |= 1<<32 - 1      // ERROR "known value of v[0-9]+ \(Or64\): -1$"
	return x == 1<<64-1 // ERROR "known value of v[0-9]+ \(Eq64\): true$"
}

func unknownBitsEq(x, y uint64) bool {
	x |= 1
	return x == y
}

func knownBitsZeroExtPassThrough(x uint8) uint64 {
	x |= 6
	return uint64(x) & 6 // ERROR "known value of v[0-9]+ \(And64\): 6$"
}

func knownBitsZeroExtUpperHalf(x uint16) uint32 {
	return uint32(x) & 0xFFFF0000 // ERROR "known value of v[0-9]+ \(And32\): 0$"
}

func unknownBitsZeroExt(x uint16) uint32 {
	x |= 0xAAAA
	return uint32(x) & 0xFFFFF000
}
