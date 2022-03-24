package main

//go:generate go run gen.go -out seqdec_amd64.s -stubs delme.go -pkg=zstd

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"

	_ "github.com/klauspost/compress"

	. "github.com/mmcloughlin/avo/build"
	"github.com/mmcloughlin/avo/buildtags"
	. "github.com/mmcloughlin/avo/operand"
	"github.com/mmcloughlin/avo/reg"
)

// insert extra checks here and there.
const debug = false

// error reported when mo == 0 && ml > 0
const errorMatchLenOfsMismatch = 1

// error reported when ml > maxMatchLen
const errorMatchLenTooBig = 2

const maxMatchLen = 131074

func main() {
	flag.Parse()
	out := flag.Lookup("out")
	os.Remove(filepath.Join("..", out.Value.String()))
	stub := flag.Lookup("stubs")
	if stub.Value.String() != "" {
		os.Remove(stub.Value.String())
		defer os.Remove(stub.Value.String())
	}

	Constraint(buildtags.Not("appengine").ToConstraint())
	Constraint(buildtags.Not("noasm").ToConstraint())
	Constraint(buildtags.Term("gc").ToConstraint())
	Constraint(buildtags.Not("noasm").ToConstraint())

	o := options{
		bmi2: false,
	}
	o.genDecodeSeqAsm("sequenceDecs_decode_amd64")
	o.bmi2 = true
	o.genDecodeSeqAsm("sequenceDecs_decode_bmi2")

	exec := executeSimple{}
	exec.generateProcedure("sequenceDecs_executeSimple_amd64")

	Generate()
	b, err := ioutil.ReadFile(out.Value.String())
	if err != nil {
		panic(err)
	}
	const readOnly = 0444
	err = ioutil.WriteFile(filepath.Join("..", out.Value.String()), b, readOnly)
	if err != nil {
		panic(err)
	}
	os.Remove(out.Value.String())
}

func debugval(v Op) {
	value := reg.R15
	MOVQ(v, value)
	INT(Imm(3))
}

func debugval32(v Op) {
	value := reg.R15L
	MOVL(v, value)
	INT(Imm(3))
}

var assertCounter int

// assert will insert code if debug is enabled.
// The code should jump to 'ok' is assertion is success.
func assert(fn func(ok LabelRef)) {
	if debug {
		caller := [100]uintptr{0}
		runtime.Callers(2, caller[:])
		frame, _ := runtime.CallersFrames(caller[:]).Next()

		ok := fmt.Sprintf("assert_check_%d_ok_srcline_%d", assertCounter, frame.Line)
		fn(LabelRef(ok))
		// Emit several since delve is imprecise.
		INT(Imm(3))
		INT(Imm(3))
		Label(ok)
		assertCounter++
	}
}

type options struct {
	bmi2 bool
}

func (o options) genDecodeSeqAsm(name string) {
	Package("github.com/klauspost/compress/zstd")
	TEXT(name, 0, "func(s *sequenceDecs, br *bitReader, ctx *decodeAsmContext) int")
	Doc(name+" decodes a sequence", "")
	Pragma("noescape")

	brValue := GP64()
	brBitsRead := GP64()
	brOffset := GP64()
	llState := GP64()
	mlState := GP64()
	ofState := GP64()
	seqBase := GP64()

	// 1. load bitReader (done once)
	brPointerStash := AllocLocal(8)
	{
		br := Dereference(Param("br"))
		brPointer := GP64()
		Load(br.Field("value"), brValue)
		Load(br.Field("bitsRead"), brBitsRead)
		Load(br.Field("off"), brOffset)
		Load(br.Field("in").Base(), brPointer)
		ADDQ(brOffset, brPointer) // Add current offset to read pointer.
		MOVQ(brPointer, brPointerStash)
	}
	{
		ctx := Dereference(Param("ctx"))
		Load(ctx.Field("llState"), llState)
		Load(ctx.Field("mlState"), mlState)
		Load(ctx.Field("ofState"), ofState)
		Load(ctx.Field("seqs").Base(), seqBase)
	}

	moP := Mem{Base: seqBase, Disp: 2 * 8} // Pointer to current mo
	mlP := Mem{Base: seqBase, Disp: 1 * 8} // Pointer to current ml
	llP := Mem{Base: seqBase, Disp: 0 * 8} // Pointer to current ll

	// MAIN LOOP:
	Label(name + "_main_loop")

	{
		brPointer := GP64()
		MOVQ(brPointerStash, brPointer)
		Comment("Fill bitreader to have enough for the offset.")
		o.bitreaderFill(name+"_fill", brValue, brBitsRead, brOffset, brPointer)

		Comment("Update offset")
		o.updateLength(name+"_of_update", brValue, brBitsRead, ofState, moP)

		// Refill if needed.
		Comment("Fill bitreader for match and literal")
		o.bitreaderFill(name+"_fill_2", brValue, brBitsRead, brOffset, brPointer)

		Comment("Update match length")
		o.updateLength(name+"_ml_update", brValue, brBitsRead, mlState, mlP)

		Comment("Update literal length")
		o.updateLength(name+"_ll_update", brValue, brBitsRead, llState, llP)

		Comment("Fill bitreader for state updates")
		o.bitreaderFill(name+"_fill_3", brValue, brBitsRead, brOffset, brPointer)
		MOVQ(brPointer, brPointerStash)
	}

	R14 := GP64()
	if o.bmi2 {
		tmp := GP64()
		MOVQ(U32(8|(8<<8)), tmp)
		BEXTRQ(tmp, ofState, R14)
	} else {
		MOVQ(ofState, R14) // copy ofState, its current value is needed below
		SHRQ(U8(8), R14)   // moB (from the ofState before its update)
		MOVBQZX(R14.As8(), R14)
	}

	// Reload ctx
	ctx := Dereference(Param("ctx"))
	iteration, err := ctx.Field("iteration").Resolve()
	if err != nil {
		panic(err)
	}
	// if ctx.iteration != 0, do update
	CMPQ(iteration.Addr, U8(0))
	JZ(LabelRef(name + "_skip_update"))

	// Update states
	{
		Comment("Update Literal Length State")
		o.updateState(name+"_llState", llState, brValue, brBitsRead, "llTable")
		Comment("Update Match Length State")
		o.updateState(name+"_mlState", mlState, brValue, brBitsRead, "mlTable")
		Comment("Update Offset State")
		o.updateState(name+"_ofState", ofState, brValue, brBitsRead, "ofTable")
	}
	Label(name + "_skip_update")

	// mo = s.adjustOffset(mo, ll, moB)

	Comment("Adjust offset")

	offset := o.adjustOffset(name+"_adjust", moP, llP, R14)
	MOVQ(offset, moP) // Store offset

	Comment("Check values")
	ml := GP64()
	MOVQ(mlP, ml)
	ll := GP64()
	MOVQ(llP, ll)

	// Update length
	{
		length := GP64()
		LEAQ(Mem{Base: ml, Index: ll, Scale: 1}, length)
		s := Dereference(Param("s"))
		seqSizeP, err := s.Field("seqSize").Resolve()
		if err != nil {
			panic(err)
		}
		ADDQ(length, seqSizeP.Addr) // s.seqSize += ml + ll
	}

	// Reload ctx
	ctx = Dereference(Param("ctx"))
	litRemainP, err := ctx.Field("litRemain").Resolve()
	if err != nil {
		panic(err)
	}
	SUBQ(ll, litRemainP.Addr) // ctx.litRemain -= ll
	{
		// 	if ml > maxMatchLen {
		//		return fmt.Errorf("match len (%d) bigger than max allowed length", ml)
		//	}
		CMPQ(ml, U32(maxMatchLen))
		JA(LabelRef(name + "_error_match_len_too_big"))
	}
	{
		// 	if mo == 0 && ml > 0 {
		//		return fmt.Errorf("zero matchoff and matchlen (%d) > 0", ml)
		//	}
		TESTQ(offset, offset)
		JNZ(LabelRef(name + "_match_len_ofs_ok")) // mo != 0
		TESTQ(ml, ml)
		JNZ(LabelRef(name + "_error_match_len_ofs_mismatch"))
	}

	Label(name + "_match_len_ofs_ok")
	ADDQ(U8(24), seqBase) // sizof(seqVals) == 3*8
	ctx = Dereference(Param("ctx"))
	iterationP, err := ctx.Field("iteration").Resolve()
	if err != nil {
		panic(err)
	}

	DECQ(iterationP.Addr)
	JNS(LabelRef(name + "_main_loop"))

	// update bitreader state before returning
	br := Dereference(Param("br"))
	Store(brValue, br.Field("value"))
	Store(brBitsRead.As8(), br.Field("bitsRead"))
	Store(brOffset, br.Field("off"))

	Comment("Return success")
	o.returnWithCode(0)

	Comment("Return with match length error")
	Label(name + "_error_match_len_ofs_mismatch")
	o.returnWithCode(errorMatchLenOfsMismatch)

	Comment("Return with match too long error")
	Label(name + "_error_match_len_too_big")
	o.returnWithCode(errorMatchLenTooBig)
}

func (o options) returnWithCode(returnCode uint32) {
	a, err := ReturnIndex(0).Resolve()
	if err != nil {
		panic(err)
	}
	MOVQ(U32(returnCode), a.Addr)
	RET()
}

func (o options) bitreaderFill(name string, brValue, brBitsRead, brOffset, brPointer reg.GPVirtual) {
	// bitreader_fill begin
	CMPQ(brBitsRead, U8(32)) //  b.bitsRead < 32
	JL(LabelRef(name + "_end"))

	CMPQ(brOffset, U8(4)) //  b.off >= 4
	JL(LabelRef(name + "_byte_by_byte"))

	// Label(name + "_fast")
	SHLQ(U8(32), brValue) // b.value << 32 | uint32(mem)
	SUBQ(U8(4), brPointer)
	SUBQ(U8(4), brOffset)
	SUBQ(U8(32), brBitsRead)
	tmp := GP64()
	MOVLQZX(Mem{Base: brPointer}, tmp)
	ORQ(tmp, brValue)
	JMP(LabelRef(name + "_end"))

	Label(name + "_byte_by_byte")
	CMPQ(brOffset, U8(0)) /* for b.off > 0 */
	JLE(LabelRef(name + "_end"))

	SHLQ(U8(8), brValue) /* b.value << 8 | uint8(mem) */
	SUBQ(U8(1), brPointer)
	SUBQ(U8(1), brOffset)
	SUBQ(U8(8), brBitsRead)

	tmp = GP64()
	MOVBQZX(Mem{Base: brPointer}, tmp)
	ORQ(tmp, brValue)

	JMP(LabelRef(name + "_byte_by_byte"))

	Label(name + "_end")
}

func (o options) updateLength(name string, brValue, brBitsRead, state reg.GPVirtual, out Mem) {
	if o.bmi2 {
		DX := GP64()
		extr := GP64()
		MOVQ(U32(8|(8<<8)), extr)
		BEXTRQ(extr, state, DX) // addBits = (state >> 8) &xff
		BX := GP64()
		MOVQ(brValue, BX)
		// TODO: We should be able to extra bits with BEXTRQ
		CX := reg.CL
		LEAQ(Mem{Base: brBitsRead, Index: DX, Scale: 1}, CX.As64()) // CX: shift = r.bitsRead + n
		ROLQ(CX, BX)
		BZHIQ(DX.As64(), BX, BX)
		MOVQ(CX.As64(), brBitsRead) // br.bitsRead += moB
		res := GP64()               // AX
		MOVQ(state, res)
		SHRQ(U8(32), res) // AX = mo (ofState.baselineInt(), that's the higher dword of moState)
		ADDQ(BX, res)     // AX - mo + br.getBits(moB)
		MOVQ(res, out)
	} else {
		BX := GP64()
		CX := reg.CL
		AX := reg.RAX
		MOVQ(state, AX.As64()) // So we can grab high bytes.
		MOVQ(brBitsRead, CX.As64())
		MOVQ(brValue, BX)
		SHLQ(CX, BX)                // BX = br.value << br.bitsRead (part of getBits)
		MOVB(AX.As8H(), CX.As8L())  // CX = moB  (ofState.addBits(), that is byte #1 of moState)
		ADDQ(CX.As64(), brBitsRead) // br.bitsRead += n (part of getBits)
		NEGL(CX.As32())             // CX = 64 - n
		SHRQ(CX, BX)                // BX = (br.value << br.bitsRead) >> (64 - n) -- getBits() result
		SHRQ(U8(32), AX)            // AX = mo (ofState.baselineInt(), that's the higher dword of moState)
		TESTQ(CX.As64(), CX.As64())
		CMOVQEQ(CX.As64(), BX) // BX is zero if n is zero

		// Check if AX is reasonable
		assert(func(ok LabelRef) {
			CMPQ(AX, U32(1<<28))
			JB(ok)
		})
		// Check if BX is reasonable
		assert(func(ok LabelRef) {
			CMPQ(BX, U32(1<<28))
			JB(ok)
		})
		ADDQ(BX, AX)  // AX - mo + br.getBits(moB)
		MOVQ(AX, out) // Store result
	}
}

func (o options) updateState(name string, state, brValue, brBitsRead reg.GPVirtual, table string) {
	name = name + "_updateState"
	AX := GP64()
	MOVBQZX(state.As8(), AX) // AX = nBits
	// Check we have a reasonable nBits
	assert(func(ok LabelRef) {
		CMPQ(AX, U8(9))
		JBE(ok)
	})

	DX := GP64()
	if o.bmi2 {
		tmp := GP64()
		MOVQ(U32(16|(16<<8)), tmp)
		BEXTRQ(tmp, state, DX)
	} else {
		MOVQ(state, DX)
		SHRQ(U8(16), DX)
		MOVWQZX(DX.As16(), DX)
	}

	{
		lowBits := o.getBits(name+"_getBits", AX, brValue, brBitsRead, LabelRef(name+"_skip_zero"))
		// Check if below tablelog
		assert(func(ok LabelRef) {
			CMPQ(lowBits, U32(512))
			JB(ok)
		})
		ADDQ(lowBits, DX)
		Label(name + "_skip_zero")
	}

	// Load table pointer
	tablePtr := GP64()
	Comment("Load ctx." + table)
	ctx := Dereference(Param("ctx"))
	tableA, err := ctx.Field(table).Base().Resolve()
	if err != nil {
		panic(err)
	}
	MOVQ(tableA.Addr, tablePtr)

	// Check if below tablelog
	assert(func(ok LabelRef) {
		CMPQ(DX, U32(512))
		JB(ok)
	})
	// Load new state
	MOVQ(Mem{Base: tablePtr, Index: DX, Scale: 8}, state)
}

// getBits will return nbits bits from brValue.
// If nbits == 0 it *may* jump to jmpZero, otherwise 0 is returned.
func (o options) getBits(name string, nBits, brValue, brBitsRead reg.GPVirtual, jmpZero LabelRef) reg.GPVirtual {
	BX := GP64()
	CX := reg.CL
	if o.bmi2 {
		LEAQ(Mem{Base: brBitsRead, Index: nBits, Scale: 1}, CX.As64())
		MOVQ(brValue, BX)
		MOVQ(CX.As64(), brBitsRead)
		ROLQ(CX, BX)
		BZHIQ(nBits, BX, BX)
	} else {
		CMPQ(nBits, U8(0))
		JZ(jmpZero)
		MOVQ(brBitsRead, CX.As64())
		ADDQ(nBits, brBitsRead)
		MOVQ(brValue, BX)
		SHLQ(CX, BX)
		MOVQ(nBits, CX.As64())
		NEGQ(CX.As64())
		SHRQ(CX, BX)
	}
	return BX
}

func (o options) adjustOffset(name string, moP, llP Mem, offsetB reg.GPVirtual) (offset reg.GPVirtual) {
	s := Dereference(Param("s"))

	po0, _ := s.Field("prevOffset").Index(0).Resolve()
	po1, _ := s.Field("prevOffset").Index(1).Resolve()
	po2, _ := s.Field("prevOffset").Index(2).Resolve()
	offset = GP64()
	MOVQ(moP, offset)
	{
		// if offsetB > 1 {
		//     s.prevOffset[2] = s.prevOffset[1]
		//     s.prevOffset[1] = s.prevOffset[0]
		//     s.prevOffset[0] = offset
		//     return offset
		// }
		CMPQ(offsetB, U8(1))
		JBE(LabelRef(name + "_offsetB_1_or_0"))

		tmp := XMM()
		MOVUPS(po0.Addr, tmp)  // tmp = (s.prevOffset[0], s.prevOffset[1])
		MOVQ(offset, po0.Addr) // s.prevOffset[0] = offset
		MOVUPS(tmp, po1.Addr)  // s.prevOffset[1], s.prevOffset[2] = s.prevOffset[0], s.prevOffset[1]
		JMP(LabelRef(name + "_end"))
	}

	Label(name + "_offsetB_1_or_0")
	// if litLen == 0 {
	//     offset++
	// }
	{
		if true {
			CMPQ(llP, U32(0))
			JNE(LabelRef(name + "_offset_maybezero"))
			INCQ(offset)
			JMP(LabelRef(name + "_offset_nonzero"))
		} else {
			// No idea why this doesn't work:
			tmp := GP64()
			LEAQ(Mem{Base: offset, Disp: 1}, tmp)
			CMPQ(llP, U32(0))
			CMOVQEQ(tmp, offset)
		}

		// if offset == 0 {
		//     return s.prevOffset[0]
		// }
		{
			Label(name + "_offset_maybezero")
			TESTQ(offset, offset)
			JNZ(LabelRef(name + "_offset_nonzero"))
			MOVQ(po0.Addr, offset)
			JMP(LabelRef(name + "_end"))
		}
	}
	Label(name + "_offset_nonzero")
	{
		// if offset == 3 {
		//     temp = s.prevOffset[0] - 1
		// } else {
		//     temp = s.prevOffset[offset]
		// }
		//
		// this if got transformed into:
		//
		// ofs   := offset
		// shift := 0
		// if offset == 3 {
		//     ofs   = 0
		//     shift = -1
		// }
		// temp := s.prevOffset[ofs] + shift
		// TODO: This should be easier...
		CX, DX, R15 := GP64(), GP64(), GP64()
		MOVQ(offset, CX)
		XORQ(DX, DX)
		MOVQ(I32(-1), R15)
		CMPQ(offset, U8(3))
		CMOVQEQ(DX, CX)
		CMOVQEQ(R15, DX)
		prevOffset := GP64()
		LEAQ(po0.Addr, prevOffset) // &prevOffset[0]
		ADDQ(Mem{Base: prevOffset, Index: CX, Scale: 8}, DX)
		temp := DX
		// if temp == 0 {
		//     temp = 1
		// }
		JNZ(LabelRef(name + "_temp_valid"))
		MOVQ(U32(1), temp)

		Label(name + "_temp_valid")
		// if offset != 1 {
		//     s.prevOffset[2] = s.prevOffset[1]
		// }
		CMPQ(offset, U8(1))
		JZ(LabelRef(name + "_skip"))
		tmp := GP64()
		MOVQ(po1.Addr, tmp)
		MOVQ(tmp, po2.Addr) // s.prevOffset[2] = s.prevOffset[1]

		Label(name + "_skip")
		// s.prevOffset[1] = s.prevOffset[0]
		// s.prevOffset[0] = temp
		tmp = GP64()
		MOVQ(po0.Addr, tmp)
		MOVQ(tmp, po1.Addr)  // s.prevOffset[1] = s.prevOffset[0]
		MOVQ(temp, po0.Addr) // s.prevOffset[0] = temp
		MOVQ(temp, offset)   // return temp
	}
	Label(name + "_end")
	return offset
}

type executeSimple struct{}

// copySize returns register size used to fast copy.
//
// See copyMemory()
func (e executeSimple) copySize() int {
	return 16
}

func (e executeSimple) generateProcedure(name string) {
	Package("github.com/klauspost/compress/zstd")
	TEXT(name, 0, "func (ctx *executeAsmContext) bool")
	Doc(name+" implements the main loop of sequenceDecs.decode in x86 asm", "")
	Pragma("noescape")

	seqsBase := GP64()
	seqsLen := GP64()
	seqIndex := GP64()
	outBase := GP64()
	outLen := GP64()
	literals := GP64()
	outPosition := GP64()
	windowSize := GP64()

	{
		ctx := Dereference(Param("ctx"))
		Load(ctx.Field("seqs").Len(), seqsLen)
		TESTQ(seqsLen, seqsLen)
		JZ(LabelRef("empty_seqs"))
		Load(ctx.Field("seqs").Base(), seqsBase)
		Load(ctx.Field("seqIndex"), seqIndex)
		Load(ctx.Field("out").Base(), outBase)
		Load(ctx.Field("out").Len(), outLen)
		Load(ctx.Field("literals").Base(), literals)
		Load(ctx.Field("outPosition"), outPosition)
		Load(ctx.Field("windowSize"), windowSize)

		tmp := GP64()
		Comment("seqsBase += 24 * seqIndex")
		LEAQ(Mem{Base: seqIndex, Index: seqIndex, Scale: 2}, tmp) // * 3
		SHLQ(U8(3), tmp)                                          // * 8
		ADDQ(tmp, seqsBase)

		Comment("outBase += outPosition")
		ADDQ(outPosition, outBase)
	}

	Label("main_loop")

	ml := GP64()
	mo := GP64()
	ll := GP64()

	moPtr := Mem{Base: seqsBase, Disp: 2 * 8}
	mlPtr := Mem{Base: seqsBase, Disp: 1 * 8}
	llPtr := Mem{Base: seqsBase, Disp: 0 * 8}

	MOVQ(mlPtr, ml)
	MOVQ(llPtr, ll)

	Comment("Copy literals")
	Label("copy_literals")
	{
		TESTQ(ll, ll)
		JZ(LabelRef("copy_match"))
		e.copyMemory("1", literals, outBase, ll)

		ADDQ(ll, literals)
		ADDQ(ll, outBase)
		ADDQ(ll, outPosition)
	}

	Comment("Copy match")
	Label("copy_match")
	{
		TESTQ(ml, ml)
		JZ(LabelRef("handle_loop"))

		MOVQ(moPtr, mo)

		Comment("Malformed input if seq.mo > t || seq.mo > s.windowSize)")
		CMPQ(mo, outPosition)
		JG(LabelRef("error_match_off_to_big"))
		CMPQ(mo, windowSize)
		JG(LabelRef("error_match_off_to_big"))

		src := GP64()
		MOVQ(outBase, src)
		SUBQ(mo, src) // src = &s.out[t - mo]

		// start := t - mo
		// if ml <= t-start {
		//     // no overlap
		// } else {
		//     // overlapping copy
		// }
		//
		// Note: ml <= t - start
		//       ml <= t - (t - mo)
		//       ml <= mo
		Comment("ml <= mo")
		CMPQ(ml, mo)
		JA(LabelRef("copy_overlapping_match"))

		Comment("Copy non-overlapping match")
		{
			e.copyMemory("2", src, outBase, ml)
			ADDQ(ml, outBase)
			ADDQ(ml, outPosition)
			JMP(LabelRef("handle_loop"))
		}

		Comment("Copy overlapping match")
		Label("copy_overlapping_match")
		{
			e.copyOverlappedMemory("3", src, outBase, ml)
			ADDQ(ml, outBase)
			ADDQ(ml, outPosition)
		}
	}

	Label("handle_loop")
	ADDQ(U8(24), seqsBase) // seqs += sizeof(seqVals)
	INCQ(seqIndex)
	CMPQ(seqIndex, seqsLen)
	JB(LabelRef("main_loop"))

	ret, err := ReturnIndex(0).Resolve()
	if err != nil {
		panic(err)
	}

	returnValue := func(val int) {

		Comment("Return value")
		MOVB(U8(val), ret.Addr)

		Comment("Update the context")
		ctx := Dereference(Param("ctx"))
		Store(seqIndex, ctx.Field("seqIndex"))
		Store(outPosition, ctx.Field("outPosition"))

		// compute litPosition
		tmp := GP64()
		Load(ctx.Field("literals").Base(), tmp)
		SUBQ(tmp, literals) // litPosition := current - initial literals pointer
		Store(literals, ctx.Field("litPosition"))
	}
	returnValue(1)
	RET()

	Label("error_match_off_to_big")
	returnValue(0)
	RET()

	Label("empty_seqs")
	Comment("Return value")
	MOVB(U8(1), ret.Addr)
	RET()
}

// copyMemory will copy memory in blocks of 16 bytes,
// overwriting up to 15 extra bytes.
func (e executeSimple) copyMemory(suffix string, src, dst, length reg.GPVirtual) {
	label := "copy_" + suffix
	ofs := GP64()
	s := Mem{Base: src, Index: ofs, Scale: 1}
	d := Mem{Base: dst, Index: ofs, Scale: 1}

	XORQ(ofs, ofs)
	Label(label)
	t := XMM()
	MOVUPS(s, t)
	MOVUPS(t, d)
	ADDQ(U8(e.copySize()), ofs)
	CMPQ(ofs, length)
	JB(LabelRef(label))
}

// copyOverlappedMemory will copy one byte at the time from src to dst.
func (e executeSimple) copyOverlappedMemory(suffix string, src, dst, length reg.GPVirtual) {
	label := "copy_slow_" + suffix
	ofs := GP64()
	s := Mem{Base: src, Index: ofs, Scale: 1}
	d := Mem{Base: dst, Index: ofs, Scale: 1}
	t := GP64()

	XORQ(ofs, ofs)
	Label(label)
	MOVB(s, t.As8())
	MOVB(t.As8(), d)
	INCQ(ofs)
	CMPQ(ofs, length)
	JB(LabelRef(label))
}
