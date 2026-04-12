// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

func (kb *knownBitsState) fold(v *Value) (value, known int64) {
	if kb.seenValues.Test(uint32(v.ID)) {
		return kb.entries[v.ID].value, kb.entries[v.ID].known
	}
	defer func() {
		// maintain the invariants:
		// 3. booleans are stored as 1 byte values who are either 0 or 1.
		if v.Type.IsBoolean() {
			value &= 1
			known |= ^1
		}

		// 2. all values are sign-extended to int64 (inspired by RISC-V's xlen=64)
		switch v.Type.Size() {
		case 1:
			value = int64(int8(value))
			known = int64(int8(known))
		case 2:
			value = int64(int16(value))
			known = int64(int16(known))
		case 4:
			value = int64(int32(value))
			known = int64(int32(known))
		case 8:
		default:
			panic("unreachable; unknown integer size")
		}

		// 1. unknown bits are always set to 0 inside value
		value &= known

		if v.Block.Func.pass.debug > 1 {
			v.Block.Func.Warnl(v.Pos, "known bits state %v: k:%d v:%d", v, known, value)
		}
		kb.entries[v.ID].known = known
		kb.entries[v.ID].value = value
	}()
	kb.seenValues.Set(uint32(v.ID)) // set seen early to give up on loops

	switch v.Op {
	// TODO: Shifts, rotates, ...
	case OpConst64, OpConst32, OpConst16, OpConst8, OpConstBool:
		return v.AuxInt, -1
	case OpAnd64, OpAnd32, OpAnd16, OpAnd8, OpAndB:
		x, xk := kb.fold(v.Args[0])
		y, yk := kb.fold(v.Args[1])
		onesInBoth := x & y
		zerosInX := ^x & xk
		zerosInY := ^y & yk
		return x & y, onesInBoth | zerosInX | zerosInY
	case OpOr64, OpOr32, OpOr16, OpOr8, OpOrB:
		x, xk := kb.fold(v.Args[0])
		y, yk := kb.fold(v.Args[1])
		zerosInBoth := ^x & ^y & (xk & yk)
		onesInX := x
		onesInY := y
		return x | y, onesInX | onesInY | zerosInBoth
	case OpXor64, OpXor32, OpXor16, OpXor8:
		x, xk := kb.fold(v.Args[0])
		y, yk := kb.fold(v.Args[1])
		return x ^ y, xk & yk
	case OpCom64, OpCom32, OpCom16, OpCom8, OpNot:
		x, xk := kb.fold(v.Args[0])
		return ^x, xk
	case OpPhi:
		set := false
		for i, arg := range v.Args {
			if !kb.isLiveInEdge(v.Block, uint(i)) {
				continue
			}
			a, k := kb.fold(arg)
			if !set {
				value, known = a, k
				set = true
			} else {
				known &^= value ^ a
				known &= k
			}
			if known == 0 {
				break
			}
		}
		return value, known
	case OpCopy, OpCvtBoolToUint8,
		OpSignExt8to16, OpSignExt8to32, OpSignExt8to64, OpSignExt16to32, OpSignExt16to64, OpSignExt32to64:
		return kb.fold(v.Args[0])
	case OpEq64, OpEq32, OpEq16, OpEq8, OpEqB:
		x, xk := kb.fold(v.Args[0])
		y, yk := kb.fold(v.Args[1])
		differentBits := x ^ y
		if differentBits&xk&yk != 0 {
			return 0, -1
		}
		if xk == -1 && yk == -1 {
			return boolToAuxInt(x == y), -1
		}
		return 0, -1 << 1
	case OpZeroExt8to16, OpZeroExt8to32, OpZeroExt8to64, OpZeroExt16to32, OpZeroExt16to64, OpZeroExt32to64:
		x, k := kb.fold(v.Args[0])
		srcSize := v.Args[0].Type.Size() * 8
		mask := int64(1<<srcSize - 1)
		return x & mask, k | ^mask
	default:
		return 0, 0
	}
}

// knownBits does constant folding across bitfields
func knownBits(f *Func) {
	kb := &knownBitsState{
		entries:         f.Cache.allocKnownBitsEntriesSlice(f.NumValues()),
		seenValues:      f.Cache.allocBitset(f.NumValues()),
		reachableBlocks: f.Cache.allocBitset(f.NumBlocks()),
	}
	defer f.Cache.freeKnownBitsEntriesSlice(kb.entries)
	defer f.Cache.freeBitset(kb.seenValues)
	defer f.Cache.freeBitset(kb.reachableBlocks)
	clear(kb.seenValues)
	clear(kb.entries)
	clear(kb.reachableBlocks)

	blocks := f.postorder()
	for _, b := range blocks {
		kb.reachableBlocks.Set(uint32(b.ID))
	}

	for _, b := range blocks {
		for _, v := range b.Values {
			if v.Uses == 0 || !(v.Type.IsInteger() || v.Type.IsBoolean()) {
				continue
			}
			switch v.Op {
			case OpConst64, OpConst32, OpConst16, OpConst8, OpConstBool:
				continue
			}
			val, k := kb.fold(v)
			if k != -1 {
				continue
			}
			if f.pass.debug > 0 {
				var pval any = val
				if v.Type.IsBoolean() {
					pval = val != 0
				}
				f.Warnl(v.Pos, "known value of %v (%v): %v", v, v.Op, pval)
			}
			var c *Value
			switch v.Type.Size() {
			case 1:
				if v.Type.IsBoolean() {
					c = f.ConstBool(v.Type, val != 0)
					break
				}
				c = f.ConstInt8(v.Type, int8(val))
			case 2:
				c = f.ConstInt16(v.Type, int16(val))
			case 4:
				c = f.ConstInt32(v.Type, int32(val))
			case 8:
				c = f.ConstInt64(v.Type, val)
			default:
				panic("unreachable; unknown integer size")
			}
			v.copyOf(c)
		}
	}
}

type knownBitsState struct {
	entries         []knownBitsEntry // indexed by Value.ID
	seenValues      bitset           // indexed by Value.ID (at the bit level)
	reachableBlocks bitset           // indexed by Block.ID (at the bit level)
}

type knownBitsEntry struct {
	// Two invariants:
	// 1. unknown bits are always set to 0 inside value
	// 2. all values are sign-extended to int64 (inspired by RISC-V's xlen=64)
	//    This means let's say you know an 8 bits value is 0b10??????,
	//    known = int64(int8(0b11000000))
	//    value = int64(int8(0b10000000))
	// 3. booleans are stored as 1 byte values who are either 0 or 1.
	known, value int64
}

func (kb *knownBitsState) isLiveInEdge(b *Block, index uint) bool {
	inEdge := b.Preds[index]
	return kb.isLiveOutEdge(inEdge.b, uint(inEdge.i))
}

func (kb *knownBitsState) isLiveOutEdge(b *Block, index uint) bool {
	if !kb.reachableBlocks.Test(uint32(b.ID)) {
		return false
	}

	switch b.Kind {
	case BlockFirst:
		return index == 0
	case BlockPlain, BlockIf, BlockDefer, BlockRet, BlockRetJmp, BlockExit, BlockJumpTable:
		return true
	default:
		panic("unreachable; unknown block kind")
	}
}
