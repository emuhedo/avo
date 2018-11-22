package load

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mmcloughlin/avo/internal/inst"
	"github.com/mmcloughlin/avo/internal/opcodescsv"
	"github.com/mmcloughlin/avo/internal/opcodesxml"
)

const (
	defaultCSVName        = "x86.v0.2.csv"
	defaultOpcodesXMLName = "x86_64.xml"
)

type Loader struct {
	X86CSVPath     string
	OpcodesXMLPath string

	alias map[opcodescsv.Alias]string
	order map[string]opcodescsv.OperandOrder
}

func NewLoaderFromDataDir(dir string) *Loader {
	return &Loader{
		X86CSVPath:     filepath.Join(dir, defaultCSVName),
		OpcodesXMLPath: filepath.Join(dir, defaultOpcodesXMLName),
	}
}

func (l *Loader) Load() ([]*inst.Instruction, error) {
	if err := l.init(); err != nil {
		return nil, err
	}

	// Load Opcodes XML file.
	iset, err := opcodesxml.ReadFile(l.OpcodesXMLPath)
	if err != nil {
		return nil, err
	}

	// Load opcodes XML data, grouped by Go opcode.
	im := map[string]*inst.Instruction{}
	for _, i := range iset.Instructions {
		for _, f := range i.Forms {
			if !l.include(f) {
				continue
			}

			for _, opcode := range l.gonames(f) {
				if im[opcode] == nil {
					im[opcode] = &inst.Instruction{
						Opcode:  opcode,
						Summary: i.Summary,
					}
				}
				im[opcode].Forms = append(im[opcode].Forms, l.form(opcode, f))
			}
		}
	}

	// Convert to a slice to return.
	is := make([]*inst.Instruction, 0, len(im))
	for _, i := range im {
		is = append(is, i)
	}

	return is, nil
}

func (l *Loader) init() error {
	icsv, err := opcodescsv.ReadFile(l.X86CSVPath)
	if err != nil {
		return err
	}

	l.alias, err = opcodescsv.BuildAliasMap(icsv)
	if err != nil {
		return err
	}

	// for a, op := range l.alias {
	// 	log.Printf("alias %#v -> %s", a, op)
	// }

	l.order = opcodescsv.BuildOrderMap(icsv)

	return nil
}

// include decides whether to include the instruction form in the avo listing.
// This discards some opcodes that are not supported in Go.
func (l Loader) include(f opcodesxml.Form) bool {
	// Exclude certain ISAs simply not present in Go
	for _, isa := range f.ISA {
		switch isa.ID {
		// Most of these are AMD-only.
		case "TBM", "CLZERO", "MONITORX", "FEMMS", "FMA4", "XOP", "SSE4A":
			return false
		// Incomplete support for some prefetching instructions.
		case "PREFETCH", "PREFETCHW", "PREFETCHWT1", "CLWB":
			return false
		}
	}

	// x86 csv contains a number of CMOV* instructions which are actually not valid
	// Go instructions. The valid Go forms should have different opcodes from GNU.
	// Therefore a decent "heuristic" is CMOV* instructions that do not have
	// aliases.
	if strings.HasPrefix(f.GASName, "cmov") && l.lookupAlias(f) == "" {
		return false
	}

	// Some specific exclusions.
	switch f.GASName {
	case "callq":
		return false
	}

	return true
}

func (l Loader) lookupAlias(f opcodesxml.Form) string {
	// Attempt lookup with datasize.
	k := opcodescsv.Alias{
		Opcode:      f.GASName,
		DataSize:    datasize(f),
		NumOperands: len(f.Operands),
	}
	if a := l.alias[k]; a != "" {
		return a
	}

	// Fallback to unknown datasize.
	k.DataSize = 0
	return l.alias[k]
}

func (l Loader) gonames(f opcodesxml.Form) []string {
	// Return alias if available.
	if a := l.lookupAlias(f); a != "" {
		return []string{a}
	}

	// Some odd special cases.
	// TODO(mbm): can this be handled by processing csv entries with slashes /
	if f.GoName == "RET" && len(f.Operands) == 1 {
		return []string{"RETFW", "RETFL", "RETFQ"}
	}

	// Use go opcode from Opcodes XML where available.
	if f.GoName != "" {
		return []string{f.GoName}
	}

	// Fallback to GAS name.
	n := strings.ToUpper(f.GASName)

	// Some need data sizes added to them.
	// TODO(mbm): is there a better way of determining which ones these are?
	s := datasize(f)
	suffix := map[int]string{16: "W", 32: "L", 64: "Q", 128: "X", 256: "Y"}
	switch n {
	case "VCVTUSI2SS", "VCVTSD2USI", "VCVTSS2USI", "VCVTUSI2SD", "VCVTTSS2USI", "VCVTTSD2USI":
		fallthrough
	case "RDRAND", "RDSEED", "MOVBEQ":
		n += suffix[s]
	}

	return []string{n}
}

func (l Loader) form(opcode string, f opcodesxml.Form) inst.Form {
	// Map operands to avo format and ensure correct order.
	ops := operands(f.Operands)

	switch l.order[opcode] {
	case opcodescsv.IntelOrder:
		// Nothing to do.
	case opcodescsv.CMP3Order:
		ops[0], ops[1] = ops[1], ops[0]
	case opcodescsv.UnknownOrder:
		// Instructions not in x86 CSV are assumed to have reverse intel order.
		fallthrough
	case opcodescsv.ReverseIntelOrder:
		for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
			ops[l], ops[r] = ops[r], ops[l]
		}
	}

	// Handle some exceptions.
	// TODO(mbm): consider if there's some nicer way to handle the list of special cases.
	switch opcode {
	// Go assembler has an internal Yu2 operand type for unsigned 2-bit immediates.
	//
	// Reference: https://github.com/golang/go/blob/6d5caf38e37bf9aeba3291f1f0b0081f934b1187/src/cmd/internal/obj/x86/asm6.go#L109
	//
	//		Yu2 // $x, x fits in uint2
	//
	// Reference: https://github.com/golang/go/blob/6d5caf38e37bf9aeba3291f1f0b0081f934b1187/src/cmd/internal/obj/x86/asm6.go#L858-L864
	//
	//	var yextractps = []ytab{
	//		{Zibr_m, 2, argList{Yu2, Yxr, Yml}},
	//	}
	//
	//	var ysha1rnds4 = []ytab{
	//		{Zibm_r, 2, argList{Yu2, Yxm, Yxr}},
	//	}
	//
	case "SHA1RNDS4", "EXTRACTPS":
		ops[0].Type = "imm2u"
	}

	return inst.Form{
		Operands: ops,
	}
}

// operands maps Opcodes XML operands to avo format. Returned in Intel order.
func operands(ops []opcodesxml.Operand) []inst.Operand {
	n := len(ops)
	r := make([]inst.Operand, n)
	for i, op := range ops {
		r[i] = operand(op)
	}
	return r
}

// operand maps an Opcodes XML operand to avo format.
func operand(op opcodesxml.Operand) inst.Operand {
	return inst.Operand{
		Type:   op.Type,
		Action: inst.ActionFromReadWrite(op.Input, op.Output),
	}
}

// datasize (intelligently) guesses the datasize of an instruction form.
func datasize(f opcodesxml.Form) int {
	// Determine from encoding bits.
	e := f.Encoding
	switch {
	case e.VEX != nil && e.VEX.W == nil:
		return 128 << e.VEX.L
	}

	// Guess from operand types.
	size := 0
	for _, op := range f.Operands {
		s := operandsize(op)
		if s != 0 && (size == 0 || op.Output) {
			size = s
		}
	}

	return size
}

func operandsize(op opcodesxml.Operand) int {
	for s := 256; s >= 8; s /= 2 {
		if strings.HasSuffix(op.Type, strconv.Itoa(s)) {
			return s
		}
	}
	return 0
}