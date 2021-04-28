// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Adapted from original by Alessandro Arzilli, used by permission.

package main

import (
	"debug/dwarf"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/ioutil"
	"os"
	"runtime"
	"sort"
	"strings"
	"unsafe"

	"github.com/go-delve/delve/pkg/dwarf/op"
	"github.com/go-delve/delve/pkg/dwarf/reader"
	"github.com/go-delve/delve/pkg/proc"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

type Function struct {
	Name       string
	Entry, End uint64 // same as DW_AT_lowpc and DW_AT_highpc
	offset     dwarf.Offset
	cu         uintptr
}

var fileCache = map[string][]string{}

func getFile(path string) []string {
	if r, cached := fileCache[path]; cached {
		return r
	}

	buf, err := ioutil.ReadFile(path)
	if err != nil {
		return nil
	}
	fileCache[path] = strings.Split(string(buf), "\n")
	return fileCache[path]
}

var verbose bool
var errors bool

type argsinfo struct {
	nFunctions    int
	argumentError int
	tooManyPieces int
	missingSource int
	wrongOrder    int
	missingDwarf  int
	duplicated    int
}

func main() {
	flag.BoolVar(&verbose, "v", verbose, "Say more about what is found")
	flag.BoolVar(&errors, "e", errors, "Report more detail for errors")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
For a binary, %s reports the fraction of function arguments
that are described at the function's "stop at" PC.
`, os.Args[0])
	}

	flag.Parse()
	if len(flag.Args()) == 0 {
		flag.Usage()
		fmt.Fprintf(os.Stderr, "\nNo input file was provided.\n")
		return
	}

	a := &argsinfo{}

	bi := proc.NewBinaryInfo(runtime.GOOS, runtime.GOARCH)
	bi.LoadBinaryInfo(flag.Args()[0], 0, []string{})

	count := 0
	countWithSortableArgs := 0

	rdr := bi.Images[0].DwarfReader()
	rdr.Seek(0)

	for _, fn := range bi.Functions {
		if fn.Entry == 0 {
			continue
		}
		file, line, _ := bi.PCToLine(fn.Entry)

		if file == "" || file == "<autogenerated>" {
			continue
		}

		if line < 0 {
			continue
		}

		lines := getFile(file)
		if len(lines) == 0 {
			fmt.Printf("\tWARNING: SOURCE FILE NOT FOUND (%s in %s)\n", fn.Name, file)
			continue
		}
		if line >= len(lines) {
			fmt.Printf("\tWARNING: LINE %d EXCEEDS RANGE %d (%s in %s)\n", line, len(lines)-1, fn.Name, file)
			continue
		}
		dclln := strings.TrimSpace(lines[line-1])

		if !strings.Contains(dclln, "func ") {
			continue
		}

		if verbose {
			fmt.Printf("function: %s\n", fn.Name)
			fmt.Printf("\tDeclaration: %s\n", dclln)
		}
		a.nFunctions++
		count++

		sourceArgs, err := getSourceArgs(dclln)
		if err != nil {
			fmt.Printf("\tWARNING: COULD NOT PARSE (%s in %s, err = %v)\n", fn.Name, file, err)
			continue
		}

		_fn := (*Function)(unsafe.Pointer(&fn))

		pc := fn.PrologueEndPC()

		if verbose {
			fmt.Printf("\tprologue ends at %#x (entry: %#x)\n", pc, fn.Entry)
		}

		dwarfArgs, ok := a.orderArgsDwarf(bi, rdr, _fn.offset, pc)
		if !ok {
			if verbose || errors {
				fmt.Printf("\tERROR: ARGS FAILED (%s in %s)\n", fn.Name, file)
			}
			continue
		}

		if verbose {
			fmt.Printf("\tDWARF arguments:\t%v\n", dwarfArgs)
			fmt.Printf("\tSource arguments:\t%v\n", sourceArgs)
		}
		countWithSortableArgs++

		if len(dwarfArgs) > len(sourceArgs) {
			a.missingSource++
			if verbose || errors {
				fmt.Printf("\tERROR: MISSING SOURCE ARGS (%s in %s, dwarfArgs=%v, sourceArgs=%v)\n", fn.Name, file, dwarfArgs, sourceArgs)
			}
			continue
		}

		if len(dwarfArgs) < len(sourceArgs) {
			a.missingDwarf++
			if verbose || errors {

				fmt.Printf("\tERROR: MISSING DWARF ARGS (%s in %s, dwarfArgs=%v, sourceArgs=%v)\n", fn.Name, file, dwarfArgs, sourceArgs)
			}
			continue
		}

		for i := range dwarfArgs {
			if dwarfArgs[i] != sourceArgs[i] {
				a.wrongOrder++
				if verbose || errors {
					fmt.Printf("\tERROR: ARGUMENT ORDER MISMATCH (%s in %s, %v vs %v)\n", fn.Name, file, dwarfArgs, sourceArgs)
				}
				break
			}
		}
	}

	if verbose {
		fmt.Printf("non-inlined non-autogenerated: %d / %d\n", count, len(bi.Functions))
		fmt.Printf("with sortable args: %d / %d\n", countWithSortableArgs, len(bi.Functions))
	}

	// type argsinfo struct {
	// 	nFunctions    int
	// 	argumentError int
	// 	tooManyPieces int
	// 	missingSource int
	// 	wrongOrder    int
	// 	missingDwarf  int
	// 	duplicated    int
	// }

	fmt.Printf("nFunctions,argumentError,tooManyPieces,missingSource,wrongOrder,missingDwarf,duplicated,1-totalErrors/nFunctions\n")
	total := a.argumentError + a.tooManyPieces + a.missingSource + a.wrongOrder + a.missingDwarf + a.duplicated
	fmt.Printf("%d,%d,%d,%d,%d,%d,%d,%f\n", a.nFunctions, a.argumentError, a.tooManyPieces, a.missingSource, a.wrongOrder, a.missingDwarf, a.duplicated, 1.0 - float64(total)/float64(a.nFunctions))

}

type arg struct {
	name string
	addr int64
}

func (a *argsinfo) orderArgsDwarf(bi *proc.BinaryInfo, rdr *reader.Reader, offset dwarf.Offset, pc uint64) ([]string, bool) {
	rdr.Seek(offset)
	rdr.Next()

	const _cfa = 0x1000

	args := []arg{}
	failed := false

	for {
		e, err := rdr.Next()
		if err != nil {
			must(err)
		}
		if e == nil || e.Tag == 0 {
			break
		}
		rdr.SkipChildren()
		if e.Tag != dwarf.TagFormalParameter {
			continue
		}

		if e.Val(dwarf.AttrName) == nil {
			continue
		}
		name := e.Val(dwarf.AttrName).(string)
		isvar := e.Val(dwarf.AttrVarParam).(bool)

		if isvar && len(name) > 0 && name[0] == '~' {
			continue
		}

		// skip all return arguments
		if isvar {
			continue
		}

		addr, pieces, _, err := bi.Location(e, dwarf.AttrLocation, pc, op.DwarfRegisters{CFA: _cfa, FrameBase: _cfa})
		if err != nil {
			a.argumentError++
			if verbose || errors {
				fmt.Printf("\targument error for %s: %v", name, err)
			}
			failed = true
			break
		}
		if len(pieces) != 0 {
			duplicatesSeen := false
			addr, pieces, duplicatesSeen = coalescePieces(pieces)
			if duplicatesSeen {
				if verbose || errors {
					fmt.Printf("\tduplicates seen %s, %v", name, pieces)
				}
				a.duplicated++
				failed = true
				break
			}

		}
		if len(pieces) != 0 {
			a.tooManyPieces++
			if verbose || errors {
				fmt.Printf("\ttoo many pieces %s, %v", name, pieces)
			}
			failed = true
			break
		}

		args = append(args, arg{e.Val(dwarf.AttrName).(string), addr})
	}

	sort.Slice(args, func(i, j int) bool {
		return args[i].addr < args[j].addr
	})

	if failed {
		return nil, false
	}

	r := make([]string, len(args))

	for i := range args {
		r[i] = args[i].name
	}

	return r, true
}

func coalescePieces(pieces []op.Piece) (int64, []op.Piece, bool) {
	duplicatesSeen := false
	sort.SliceStable(pieces, func(i,j int) bool {return pieces[i].Addr < pieces[j].Addr} )
	j := 1
	for i := 1; i < len(pieces); i++ {
		if pieces[i-1] == pieces[i] {
			duplicatesSeen = true
			continue
		}
		pieces[j] = pieces[i]
		j++
	}
	pieces = pieces[:j]
	r := append(make([]op.Piece, 0, len(pieces)), pieces[0])

	for i := 1; i < len(pieces); i++ {
		if r[len(r)-1].Addr+int64(r[len(r)-1].Size) == pieces[i].Addr { // && !r[len(r)-1].IsRegister && !pieces[i].IsRegister {
			r[len(r)-1].Size += pieces[i].Size
		} else {
			r = append(r, pieces[i])
		}
	}

	if len(r) == 1 { // && !r[0].IsRegister {
		return r[0].Addr, nil, duplicatesSeen
	}

	return 0, pieces, duplicatesSeen
}

func getSourceArgs(dclln string) ([]string, error) {
	if dclln[len(dclln)-1] != '}' {
		dclln = dclln + "\n}"
	}

	source := []byte(fmt.Sprintf("package F; %s", dclln))

	var fset token.FileSet
	f, err := parser.ParseFile(&fset, "in", source, parser.AllErrors)
	if err != nil {
		return nil, err
	}

	var v getSourceArgsVisitor
	ast.Walk(&v, f)

	return v.out, nil
}

type getSourceArgsVisitor struct {
	out []string
}

func (v *getSourceArgsVisitor) Visit(node ast.Node) ast.Visitor {
	fn, ok := node.(*ast.FuncDecl)
	if !ok {
		return v
	}
	//for _, lst := range []*ast.FieldList{fn.Recv, fn.Type.Params , fn.Type.Results} {
	for _, lst := range []*ast.FieldList{fn.Recv, fn.Type.Params /*skip all return arguments*/} {
		if lst == nil {
			continue
		}
		cnt := 0
		for _, field := range lst.List {
			for _, name := range field.Names {
				if name == nil {
					v.out = append(v.out, fmt.Sprintf("~r%d", cnt))
				} else if name.Name != "_" {
					v.out = append(v.out, name.Name)
				}
				cnt++
			}
		}
	}
	return nil
}
