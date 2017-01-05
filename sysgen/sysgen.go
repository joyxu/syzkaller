// Copyright 2015/2016 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	. "github.com/google/syzkaller/sysparser"
)

var (
	flagV = flag.Int("v", 0, "verbosity")
)

const (
	ptrSize = 8
)

func main() {
	flag.Parse()

	inputFiles, err := filepath.Glob("sys/*\\.txt")
	if err != nil {
		failf("failed to find input files: %v", err)
	}
	var r io.Reader = bytes.NewReader(nil)
	for _, f := range inputFiles {
		inf, err := os.Open(f)
		logf(1, "Load descriptions from file %v", f)
		if err != nil {
			failf("failed to open input file: %v", err)
		}
		defer inf.Close()
		r = io.MultiReader(r, bufio.NewReader(inf))
	}

	logf(1, "Parse system call descriptions")
	desc := Parse(r)

	consts := make(map[string]map[string]uint64)
	for _, arch := range archs {
		logf(0, "generating %v...", arch.Name)
		consts[arch.Name] = readConsts(arch.Name)

		unsupported := make(map[string]bool)
		archFlags := make(map[string][]string)
		for f, vals := range desc.Flags {
			var archVals []string
			for _, val := range vals {
				if isIdentifier(val) {
					if v, ok := consts[arch.Name][val]; ok {
						archVals = append(archVals, fmt.Sprint(v))
					} else {
						if !unsupported[val] {
							unsupported[val] = true
							logf(0, "unsupported flag: %v", val)
						}
					}
				} else {
					archVals = append(archVals, val)
				}
			}
			archFlags[f] = archVals
		}

		sysFile := filepath.Join("sys", "sys_"+arch.Name+".go")
		logf(1, "Generate code to init system call data in %v", sysFile)
		out := new(bytes.Buffer)
		archDesc := *desc
		archDesc.Flags = archFlags
		generate(arch.Name, &archDesc, consts[arch.Name], out)
		writeSource(sysFile, out.Bytes())
		logf(0, "")
	}

	generateExecutorSyscalls(desc.Syscalls, consts)
}

func readConsts(arch string) map[string]uint64 {
	constFiles, err := filepath.Glob("sys/*_" + arch + ".const")
	if err != nil {
		failf("failed to find const files: %v", err)
	}
	consts := make(map[string]uint64)
	for _, fname := range constFiles {
		f, err := os.Open(fname)
		if err != nil {
			failf("failed to open const file: %v", err)
		}
		defer f.Close()
		s := bufio.NewScanner(f)
		for s.Scan() {
			line := s.Text()
			if line == "" || line[0] == '#' {
				continue
			}
			eq := strings.IndexByte(line, '=')
			if eq == -1 {
				failf("malformed const file %v: no '=' in '%v'", fname, line)
			}
			name := strings.TrimSpace(line[:eq])
			val, err := strconv.ParseUint(strings.TrimSpace(line[eq+1:]), 0, 64)
			if err != nil {
				failf("malformed const file %v: bad value in '%v'", fname, line)
			}
			if old, ok := consts[name]; ok && old != val {
				failf("const %v has different values for %v: %v vs %v", name, arch, old, val)
			}
			consts[name] = val
		}
		if err := s.Err(); err != nil {
			failf("failed to read const file: %v", err)
		}
	}
	for name, nr := range syzkalls {
		consts["__NR_"+name] = nr
	}
	return consts
}

var skipCurrentSyscall string

func skipSyscall(why string) {
	if skipCurrentSyscall != "" {
		skipCurrentSyscall = why
	}
}

func generate(arch string, desc *Description, consts map[string]uint64, out io.Writer) {
	unsupported := make(map[string]bool)

	fmt.Fprintf(out, "// AUTOGENERATED FILE\n")
	fmt.Fprintf(out, "package sys\n\n")

	generateResources(desc, consts, out)
	generateStructs(desc, consts, out)

	fmt.Fprintf(out, "func initCalls() {\n")
	for _, s := range desc.Syscalls {
		logf(4, "    generate population code for %v", s.Name)
		skipCurrentSyscall = ""
		syscallNR := -1
		if nr, ok := consts["__NR_"+s.CallName]; ok {
			syscallNR = int(nr)
		} else {
			if !unsupported[s.CallName] {
				unsupported[s.CallName] = true
				logf(0, "unsupported syscall: %v", s.CallName)
			}
		}
		fmt.Fprintf(out, "func() { Calls = append(Calls, &Call{Name: \"%v\", CallName: \"%v\"", s.Name, s.CallName)
		if len(s.Ret) != 0 {
			fmt.Fprintf(out, ", Ret: ")
			generateArg("", "ret", s.Ret[0], "out", s.Ret[1:], desc, consts, true, false, out)
		}
		fmt.Fprintf(out, ", Args: []Type{")
		for i, a := range s.Args {
			if i != 0 {
				fmt.Fprintf(out, ", ")
			}
			logf(5, "      generate description for arg %v", i)
			generateArg("", a[0], a[1], "in", a[2:], desc, consts, true, false, out)
		}
		if skipCurrentSyscall != "" {
			logf(0, "unsupported syscall: %v due to %v", s.Name, skipCurrentSyscall)
			syscallNR = -1
		}
		fmt.Fprintf(out, "}, NR: %v})}()\n", syscallNR)
	}
	fmt.Fprintf(out, "}\n\n")

	var constArr []NameValue
	for name, val := range consts {
		constArr = append(constArr, NameValue{name, val})
	}
	sort.Sort(NameValueArray(constArr))

	fmt.Fprintf(out, "const (\n")
	for _, nv := range constArr {
		fmt.Fprintf(out, "%v = %v\n", nv.name, nv.val)
	}
	fmt.Fprintf(out, ")\n")
}

func generateResources(desc *Description, consts map[string]uint64, out io.Writer) {
	var resArray ResourceArray
	for _, res := range desc.Resources {
		resArray = append(resArray, res)
	}
	sort.Sort(resArray)

	fmt.Fprintf(out, "var Resources = map[string]*ResourceDesc{\n")
	for _, res := range resArray {
		underlying := ""
		name := res.Name
		kind := []string{name}
		var values []string
	loop:
		for {
			var values1 []string
			for _, v := range res.Values {
				if v1, ok := consts[v]; ok {
					values1 = append(values1, fmt.Sprint(v1))
				} else if !isIdentifier(v) {
					values1 = append(values1, v)
				}
			}
			values = append(values1, values...)
			switch res.Base {
			case "int8", "int16", "int32", "int64", "intptr":
				underlying = res.Base
				break loop
			default:
				if _, ok := desc.Resources[res.Base]; !ok {
					failf("resource '%v' has unknown parent resource '%v'", name, res.Base)
				}
				kind = append([]string{res.Base}, kind...)
				res = desc.Resources[res.Base]
			}
		}
		fmt.Fprintf(out, "\"%v\": &ResourceDesc{Name: \"%v\", Type: ", name, name)
		generateArg("", "resource-type", underlying, "inout", nil, desc, consts, true, true, out)
		fmt.Fprintf(out, ", Kind: []string{")
		for i, k := range kind {
			if i != 0 {
				fmt.Fprintf(out, ", ")
			}
			fmt.Fprintf(out, "\"%v\"", k)
		}
		fmt.Fprintf(out, "}, Values: []uintptr{")
		if len(values) == 0 {
			values = append(values, "0")
		}
		for i, v := range values {
			if i != 0 {
				fmt.Fprintf(out, ", ")
			}
			fmt.Fprintf(out, "%v", v)
		}
		fmt.Fprintf(out, "}},\n")
	}
	fmt.Fprintf(out, "}\n")
}

type structKey struct {
	name  string
	field string
	dir   string
}

func generateStructEntry(str Struct, key structKey, out io.Writer) {
	typ := "StructType"
	if str.IsUnion {
		typ = "UnionType"
	}
	name := key.field
	if name == "" {
		name = key.name
	}
	packed := ""
	if str.Packed {
		packed = ", packed: true"
	}
	varlen := ""
	if str.Varlen {
		varlen = ", varlen: true"
	}
	align := ""
	if str.Align != 0 {
		align = fmt.Sprintf(", align: %v", str.Align)
	}
	fmt.Fprintf(out, "\"%v\": &%v{TypeCommon: TypeCommon{TypeName: \"%v\", ArgDir: %v, IsOptional: %v} %v %v %v},\n",
		key, typ, name, fmtDir(key.dir), false, packed, align, varlen)
}

func generateStructFields(str Struct, key structKey, desc *Description, consts map[string]uint64, out io.Writer) {
	typ := "StructType"
	fields := "Fields"
	if str.IsUnion {
		typ = "UnionType"
		fields = "Options"
	}
	fmt.Fprintf(out, "func() { s := Structs[\"%v\"].(*%v)\n", key, typ)
	for _, a := range str.Flds {
		fmt.Fprintf(out, "s.%v = append(s.%v, ", fields, fields)
		generateArg(str.Name, a[0], a[1], key.dir, a[2:], desc, consts, false, true, out)
		fmt.Fprintf(out, ")\n")
	}
	fmt.Fprintf(out, "}()\n")
}

func generateStructs(desc *Description, consts map[string]uint64, out io.Writer) {
	// Struct fields can refer to other structs. Go compiler won't like if
	// we refer to Structs map during Structs map initialization. So we do
	// it in 2 passes: on the first pass create types and assign them to
	// the map, on the second pass fill in fields.

	// Since structs of the same type can be fields with different names
	// of multiple other structs, we have an instance of those structs
	// for each field indexed by the name of the parent struct and the
	// field name.

	structMap := make(map[structKey]Struct)
	for _, str := range desc.Structs {
		for _, dir := range []string{"in", "out", "inout"} {
			structMap[structKey{str.Name, "", dir}] = str
		}
		for _, a := range str.Flds {
			if innerStr, ok := desc.Structs[a[1]]; ok {
				for _, dir := range []string{"in", "out", "inout"} {
					structMap[structKey{a[1], a[0], dir}] = innerStr
				}
			}
		}
	}

	fmt.Fprintf(out, "var Structs = map[string]Type{\n")
	for key, str := range structMap {
		generateStructEntry(str, key, out)
	}
	fmt.Fprintf(out, "}\n")

	fmt.Fprintf(out, "func initStructFields() {\n")
	for key, str := range structMap {
		generateStructFields(str, key, desc, consts, out)
	}
	fmt.Fprintf(out, "}\n")
}

func parseRange(buffer string, consts map[string]uint64) (string, string) {
	lookupConst := func(name string) string {
		if v, ok := consts[name]; ok {
			return fmt.Sprint(v)
		}
		return name
	}

	parts := strings.Split(buffer, ":")
	switch len(parts) {
	case 1:
		v := lookupConst(buffer)
		return v, v
	case 2:
		return lookupConst(parts[0]), lookupConst(parts[1])
	default:
		failf("bad range: %v", buffer)
		return "", ""
	}
}

func generateArg(
	parent, name, typ, dir string,
	a []string,
	desc *Description,
	consts map[string]uint64,
	isArg, isField bool,
	out io.Writer) {
	origName := name
	name = "\"" + name + "\""
	opt := false
	for i, v := range a {
		if v == "opt" {
			opt = true
			copy(a[i:], a[i+1:])
			a = a[:len(a)-1]
			break
		}
	}
	common := func() string {
		return fmt.Sprintf("TypeCommon: TypeCommon{TypeName: %v, ArgDir: %v, IsOptional: %v}", name, fmtDir(dir), opt)
	}
	canBeArg := false
	switch typ {
	case "fileoff":
		canBeArg = true
		size := uint64(ptrSize)
		bigEndian := false
		if isField {
			if want := 1; len(a) != want {
				failf("wrong number of arguments for %v arg %v, want %v, got %v", typ, name, want, len(a))
			}
			size, bigEndian = decodeIntType(a[0])
		} else {
			if want := 0; len(a) != want {
				failf("wrong number of arguments for %v arg %v, want %v, got %v", typ, name, want, len(a))
			}
		}
		fmt.Fprintf(out, "&IntType{%v, TypeSize: %v, BigEndian: %v, Kind: IntFileoff}", common(), size, bigEndian)
	case "buffer":
		canBeArg = true
		if want := 1; len(a) != want {
			failf("wrong number of arguments for %v arg %v, want %v, got %v", typ, name, want, len(a))
		}
		ptrCommonHdr := common()
		dir = a[0]
		opt = false
		fmt.Fprintf(out, "&PtrType{%v, Type: &BufferType{%v, Kind: BufferBlobRand}}", ptrCommonHdr, common())
	case "string":
		if len(a) != 0 && len(a) != 1 && len(a) != 2 {
			failf("wrong number of arguments for %v arg %v, want 0-2, got %v", typ, name, len(a))
		}
		var vals []string
		subkind := ""
		if len(a) >= 1 {
			if a[0][0] == '"' {
				vals = append(vals, a[0][1:len(a[0])-1])
			} else {
				vals1, ok := desc.StrFlags[a[0]]
				if !ok {
					failf("unknown string flags %v", a[0])
				}
				vals = append([]string{}, vals1...)
				subkind = a[0]
			}
		}
		for i, s := range vals {
			vals[i] = s + "\x00"
		}
		if len(a) >= 2 {
			var size uint64
			if v, ok := consts[a[1]]; ok {
				size = v
			} else {
				v, err := strconv.ParseUint(a[1], 10, 64)
				if err != nil {
					failf("failed to parse string length for %v", name, a[1])
				}
				size = v
			}
			for i, s := range vals {
				if uint64(len(s)) > size {
					failf("string value %q exceeds buffer length %v for arg %v", s, size, name)
				}
				for uint64(len(s)) < size {
					s += "\x00"
				}
				vals[i] = s
			}
		}
		fmt.Fprintf(out, "&BufferType{%v, Kind: BufferString, SubKind: %q, Values: %#v}", common(), subkind, vals)
	case "salg_type":
		if want := 0; len(a) != want {
			failf("wrong number of arguments for %v arg %v, want %v, got %v", typ, name, want, len(a))
		}
		fmt.Fprintf(out, "&BufferType{%v, Kind: BufferAlgType}", common())
	case "salg_name":
		if want := 0; len(a) != want {
			failf("wrong number of arguments for %v arg %v, want %v, got %v", typ, name, want, len(a))
		}
		fmt.Fprintf(out, "&BufferType{%v, Kind: BufferAlgName}", common())
	case "vma":
		canBeArg = true
		begin, end := "0", "0"
		switch len(a) {
		case 0:
		case 1:
			begin, end = parseRange(a[0], consts)
		default:
			failf("wrong number of arguments for %v arg %v, want 0 or 1, got %v", typ, name, len(a))
		}
		fmt.Fprintf(out, "&VmaType{%v, RangeBegin: %v, RangeEnd: %v}", common(), begin, end)
	case "len", "bytesize", "bytesize2", "bytesize4", "bytesize8":
		canBeArg = true
		size := uint64(ptrSize)
		bigEndian := false
		if isField {
			if want := 2; len(a) != want {
				failf("wrong number of arguments for %v arg %v, want %v, got %v", typ, name, want, len(a))
			}
			size, bigEndian = decodeIntType(a[1])
		} else {
			if want := 1; len(a) != want {
				failf("wrong number of arguments for %v arg %v, want %v, got %v", typ, name, want, len(a))
			}
		}
		byteSize := uint8(0)
		if typ != "len" {
			byteSize = decodeByteSizeType(typ)
		}
		fmt.Fprintf(out, "&LenType{%v, Buf: \"%v\", TypeSize: %v, BigEndian: %v, ByteSize: %v}", common(), a[0], size, bigEndian, byteSize)
	case "flags":
		canBeArg = true
		size := uint64(ptrSize)
		bigEndian := false
		if isField {
			if want := 2; len(a) != want {
				failf("wrong number of arguments for %v arg %v, want %v, got %v", typ, name, want, len(a))
			}
			size, bigEndian = decodeIntType(a[1])
		} else {
			if want := 1; len(a) != want {
				failf("wrong number of arguments for %v arg %v, want %v, got %v", typ, name, want, len(a))
			}
		}
		vals, ok := desc.Flags[a[0]]
		if !ok {
			failf("unknown flag %v", a[0])
		}
		if len(vals) == 0 {
			fmt.Fprintf(out, "&IntType{%v, TypeSize: %v, BigEndian: %v}", common(), size, bigEndian)
		} else {
			fmt.Fprintf(out, "&FlagsType{%v, TypeSize: %v, BigEndian: %v, Vals: []uintptr{%v}}", common(), size, bigEndian, strings.Join(vals, ","))
		}
	case "const":
		canBeArg = true
		size := uint64(ptrSize)
		bigEndian := false
		if isField {
			if want := 2; len(a) != want {
				failf("wrong number of arguments for %v arg %v, want %v, got %v", typ, name, want, len(a))
			}
			size, bigEndian = decodeIntType(a[1])
		} else {
			if want := 1; len(a) != want {
				failf("wrong number of arguments for %v arg %v, want %v, got %v", typ, name, want, len(a))
			}
		}
		val := a[0]
		if v, ok := consts[a[0]]; ok {
			val = fmt.Sprint(v)
		} else if isIdentifier(a[0]) {
			// This is an identifier for which we don't have a value for this arch.
			// Skip this syscall on this arch.
			val = "0"
			skipSyscall(fmt.Sprintf("missing const %v", a[0]))
		}
		fmt.Fprintf(out, "&ConstType{%v, TypeSize: %v, BigEndian: %v, Val: uintptr(%v)}", common(), size, bigEndian, val)
	case "proc":
		canBeArg = true
		size := uint64(ptrSize)
		bigEndian := false
		var valuesStart string
		var valuesPerProc string
		if isField {
			if want := 3; len(a) != want {
				failf("wrong number of arguments for %v arg %v, want %v, got %v", typ, name, want, len(a))
			}
			size, bigEndian = decodeIntType(a[0])
			valuesStart = a[1]
			valuesPerProc = a[2]
		} else {
			if want := 2; len(a) != want {
				failf("wrong number of arguments for %v arg %v, want %v, got %v", typ, name, want, len(a))
			}
			valuesStart = a[0]
			valuesPerProc = a[1]
		}
		valuesStartInt, err := strconv.ParseInt(valuesStart, 10, 64)
		if err != nil {
			failf("couldn't parse '%v' as int64", valuesStart)
		}
		valuesPerProcInt, err := strconv.ParseInt(valuesPerProc, 10, 64)
		if err != nil {
			failf("couldn't parse '%v' as int64", valuesPerProc)
		}
		if valuesPerProcInt < 1 {
			failf("values per proc '%v' should be >= 1", valuesPerProcInt)
		}
		if valuesStartInt >= (1 << (size * 8)) {
			failf("values starting from '%v' overflow desired type of size '%v'", valuesStartInt, size)
		}
		const maxPids = 32 // executor knows about this constant (MAX_PIDS)
		if valuesStartInt+maxPids*valuesPerProcInt >= (1 << (size * 8)) {
			failf("not enough values starting from '%v' with step '%v' and type size '%v' for 32 procs", valuesStartInt, valuesPerProcInt, size)
		}
		fmt.Fprintf(out, "&ProcType{%v, TypeSize: %v, BigEndian: %v, ValuesStart: %v, ValuesPerProc: %v}", common(), size, bigEndian, valuesStartInt, valuesPerProcInt)
	case "int8", "int16", "int32", "int64", "intptr", "int16be", "int32be", "int64be", "intptrbe":
		canBeArg = true
		size, bigEndian := decodeIntType(typ)
		switch len(a) {
		case 0:
			fmt.Fprintf(out, "&IntType{%v, TypeSize: %v, BigEndian: %v}", common(), size, bigEndian)
		case 1:
			begin, end := parseRange(a[0], consts)
			fmt.Fprintf(out, "&IntType{%v, TypeSize: %v, BigEndian: %v, Kind: IntRange, RangeBegin: %v, RangeEnd: %v}", common(), size, bigEndian, begin, end)
		default:
			failf("wrong number of arguments for %v arg %v, want 0 or 1, got %v", typ, name, len(a))
		}
	case "signalno":
		canBeArg = true
		if want := 0; len(a) != want {
			failf("wrong number of arguments for %v arg %v, want %v, got %v", typ, name, want, len(a))
		}
		fmt.Fprintf(out, "&IntType{%v, TypeSize: 4, Kind: IntSignalno}", common())
	case "filename":
		canBeArg = true
		if want := 0; len(a) != want {
			failf("wrong number of arguments for %v arg %v, want %v, got %v", typ, name, want, len(a))
		}
		ptrCommonHdr := common()
		dir = "in"
		opt = false
		fmt.Fprintf(out, "&PtrType{%v, Type: &BufferType{%v, Kind: BufferFilename}}", ptrCommonHdr, common())
	case "array":
		if len(a) != 1 && len(a) != 2 {
			failf("wrong number of arguments for %v arg %v, want 1 or 2, got %v", typ, name, len(a))
		}
		if len(a) == 1 {
			if a[0] == "int8" {
				fmt.Fprintf(out, "&BufferType{%v, Kind: BufferBlobRand}", common())
			} else {
				fmt.Fprintf(out, "&ArrayType{%v, Type: %v, Kind: ArrayRandLen}", common(), generateType(a[0], dir, desc, consts))
			}
		} else {
			begin, end := parseRange(a[1], consts)
			if a[0] == "int8" {
				fmt.Fprintf(out, "&BufferType{%v, Kind: BufferBlobRange, RangeBegin: %v, RangeEnd: %v}", common(), begin, end)
			} else {
				fmt.Fprintf(out, "&ArrayType{%v, Type: %v, Kind: ArrayRangeLen, RangeBegin: %v, RangeEnd: %v}", common(), generateType(a[0], dir, desc, consts), begin, end)
			}
		}
	case "ptr":
		canBeArg = true
		if want := 2; len(a) != want {
			failf("wrong number of arguments for %v arg %v, want %v, got %v", typ, name, want, len(a))
		}
		dir = "in"
		fmt.Fprintf(out, "&PtrType{%v, Type: %v}", common(), generateType(a[1], a[0], desc, consts))
	default:
		if strings.HasPrefix(typ, "unnamed") {
			if inner, ok := desc.Unnamed[typ]; ok {
				generateArg("", "", inner[0], dir, inner[1:], desc, consts, false, isField, out)
			} else {
				failf("unknown unnamed type '%v'", typ)
			}
		} else if _, ok := desc.Structs[typ]; ok {
			if len(a) != 0 {
				failf("struct '%v' has args", typ)
			}
			fmt.Fprintf(out, "Structs[\"%v\"]", structKey{typ, origName, dir})
		} else if _, ok := desc.Resources[typ]; ok {
			if len(a) != 0 {
				failf("resource '%v' has args", typ)
			}
			fmt.Fprintf(out, "&ResourceType{%v, Desc: Resources[\"%v\"]}", common(), typ)
			return
		} else {
			failf("unknown arg type \"%v\" for %v", typ, name)
		}
	}
	if isArg && !canBeArg {
		failf("%v %v can't be syscall argument/return", name, typ)
	}
}

func generateType(typ, dir string, desc *Description, consts map[string]uint64) string {
	buf := new(bytes.Buffer)
	generateArg("", "", typ, dir, nil, desc, consts, false, true, buf)
	return buf.String()
}

func fmtDir(s string) string {
	switch s {
	case "in":
		return "DirIn"
	case "out":
		return "DirOut"
	case "inout":
		return "DirInOut"
	default:
		failf("bad direction %v", s)
		return ""
	}
}

func decodeIntType(typ string) (uint64, bool) {
	bigEndian := false
	if strings.HasSuffix(typ, "be") {
		bigEndian = true
		typ = typ[:len(typ)-2]
	}
	switch typ {
	case "int8", "int16", "int32", "int64", "intptr":
	default:
		failf("unknown type %v", typ)
	}
	sz := int64(ptrSize * 8)
	if typ != "intptr" {
		sz, _ = strconv.ParseInt(typ[3:], 10, 64)
	}
	return uint64(sz / 8), bigEndian
}

func decodeByteSizeType(typ string) uint8 {
	switch typ {
	case "bytesize", "bytesize2", "bytesize4", "bytesize8":
	default:
		failf("unknown type %v", typ)
	}
	sz := int64(1)
	if typ != "bytesize" {
		sz, _ = strconv.ParseInt(typ[8:], 10, 8)
	}
	return uint8(sz)
}

func isIdentifier(s string) bool {
	for i, c := range s {
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || i > 0 && (c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

func writeSource(file string, data []byte) {
	src, err := format.Source(data)
	if err != nil {
		fmt.Printf("%s\n", data)
		failf("failed to format output: %v", err)
	}
	writeFile(file, src)
}

func writeFile(file string, data []byte) {
	outf, err := os.Create(file)
	if err != nil {
		failf("failed to create output file: %v", err)
	}
	defer outf.Close()
	outf.Write(data)
}

type NameValue struct {
	name string
	val  uint64
}

type NameValueArray []NameValue

func (a NameValueArray) Len() int           { return len(a) }
func (a NameValueArray) Less(i, j int) bool { return a[i].name < a[j].name }
func (a NameValueArray) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

type ResourceArray []Resource

func (a ResourceArray) Len() int           { return len(a) }
func (a ResourceArray) Less(i, j int) bool { return a[i].Name < a[j].Name }
func (a ResourceArray) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

func failf(msg string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, msg+"\n", args...)
	os.Exit(1)
}

func logf(v int, msg string, args ...interface{}) {
	if *flagV >= v {
		fmt.Fprintf(os.Stderr, msg+"\n", args...)
	}
}
