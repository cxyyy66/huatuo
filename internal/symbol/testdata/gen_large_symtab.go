// Copyright 2026 The HuaTuo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build ignore

// Command gen_large_symtab writes production-like ELF symbol table fixtures.
package main

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

const (
	largeSymtabCount     = 560_000
	largeDynsymCount     = 56_000
	largeFilePath        = "internal/symbol/testdata/large_symtab_200m.elf"
	referenceSymtabCount = 90
	referenceDynsymCount = 10
	referenceFilePath    = "internal/symbol/testdata/symtab_100.elf"
)

// symName mirrors largeSymName in large_symtab_test.go; keep in sync.
func symName(i int) string {
	length := 100 + (i*7)%401
	name := make([]byte, length)
	copy(name, fmt.Sprintf("f%07d_", i))
	for j := 9; j < length; j++ {
		name[j] = 'a' + byte((i+j)%26)
	}
	return string(name)
}

func symType(i int, dynsym bool) elf.SymType {
	if dynsym {
		if i%5 < 3 {
			return elf.STT_FUNC
		}
		return elf.STT_OBJECT
	}
	switch i % 5 {
	case 0, 1:
		return elf.STT_FUNC
	case 2, 3:
		return elf.STT_OBJECT
	default:
		return elf.STT_NOTYPE
	}
}

func buildStringTable(count int) ([]byte, []uint32) {
	var table bytes.Buffer
	table.WriteByte(0)
	offsets := make([]uint32, count)
	for i := 0; i < count; i++ {
		offsets[i] = uint32(table.Len())
		table.WriteString(symName(i))
		table.WriteByte(0)
	}
	return table.Bytes(), offsets
}

func buildSymbolTable(offsets []uint32, count int, dynsym bool) []byte {
	symbols := make([]byte, elf.Sym64Size*(count+1))
	for i := 0; i < count; i++ {
		entry := symbols[elf.Sym64Size*(i+1):]
		binary.LittleEndian.PutUint32(entry[0:4], offsets[i])
		entry[4] = byte(symType(i, dynsym))
		binary.LittleEndian.PutUint16(entry[6:8], 1)
		binary.LittleEndian.PutUint64(entry[8:16], 0x1000+uint64(i)*0x10)
		binary.LittleEndian.PutUint64(entry[16:24], 0x10)
	}
	return symbols
}

func alignUp(value, alignment int) int {
	return (value + alignment - 1) &^ (alignment - 1)
}

func encodeStruct(value any) ([]byte, error) {
	var encoded bytes.Buffer
	if err := binary.Write(&encoded, binary.LittleEndian, value); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func main() {
	writeFixture(
		largeFilePath,
		largeSymtabCount,
		largeDynsymCount,
		[]int{1, 100_000, 500_000},
	)
	writeFixture(
		referenceFilePath,
		referenceSymtabCount,
		referenceDynsymCount,
		[]int{1, 50, 89},
	)
}

func writeFixture(filePath string, symtabCount, dynsymCount int, verifyIndexes []int) {
	dynstr, dynOffsets := buildStringTable(dynsymCount)
	dynsym := buildSymbolTable(dynOffsets, dynsymCount, true)
	strtab, symOffsets := buildStringTable(symtabCount)
	symtab := buildSymbolTable(symOffsets, symtabCount, false)

	headerSize := binary.Size(elf.Header64{})
	sectionSize := binary.Size(elf.Section64{})
	dynstrOffset := headerSize
	dynsymOffset := alignUp(dynstrOffset+len(dynstr), 8)
	strtabOffset := alignUp(dynsymOffset+len(dynsym), 8)
	symtabOffset := alignUp(strtabOffset+len(strtab), 8)
	sectionHeadersOffset := alignUp(symtabOffset+len(symtab), 8)
	sectionCount := 5
	image := make([]byte, sectionHeadersOffset+sectionCount*sectionSize)

	ident := [elf.EI_NIDENT]byte{}
	copy(ident[:], elf.ELFMAG)
	ident[elf.EI_CLASS] = byte(elf.ELFCLASS64)
	ident[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	ident[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	header := elf.Header64{
		Ident:     ident,
		Type:      uint16(elf.ET_REL),
		Machine:   uint16(elf.EM_X86_64),
		Version:   uint32(elf.EV_CURRENT),
		Shoff:     uint64(sectionHeadersOffset),
		Ehsize:    uint16(headerSize),
		Shentsize: uint16(sectionSize),
		Shnum:     uint16(sectionCount),
	}
	encodedHeader, err := encodeStruct(header)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode ELF header: %v\n", err)
		os.Exit(1)
	}
	copy(image, encodedHeader)
	copy(image[dynstrOffset:], dynstr)
	copy(image[dynsymOffset:], dynsym)
	copy(image[strtabOffset:], strtab)
	copy(image[symtabOffset:], symtab)

	headers := []elf.Section64{
		{}, // null section
		{Type: uint32(elf.SHT_STRTAB), Off: uint64(dynstrOffset), Size: uint64(len(dynstr)), Addralign: 1},
		{Type: uint32(elf.SHT_DYNSYM), Off: uint64(dynsymOffset), Size: uint64(len(dynsym)), Link: 1, Addralign: 8, Entsize: elf.Sym64Size},
		{Type: uint32(elf.SHT_STRTAB), Off: uint64(strtabOffset), Size: uint64(len(strtab)), Addralign: 1},
		{Type: uint32(elf.SHT_SYMTAB), Off: uint64(symtabOffset), Size: uint64(len(symtab)), Link: 3, Addralign: 8, Entsize: elf.Sym64Size},
	}
	for i, section := range headers {
		encoded, err := encodeStruct(section)
		if err != nil {
			fmt.Fprintf(os.Stderr, "encode section %d header: %v\n", i, err)
			os.Exit(1)
		}
		copy(image[sectionHeadersOffset+i*sectionSize:], encoded)
	}

	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "MkdirAll: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filePath, image, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "WriteFile(%q): %v\n", filePath, err)
		os.Exit(1)
	}

	verify(image, symtabCount, dynsymCount, verifyIndexes)
	fmt.Printf("wrote %s (%d bytes, %d symtab + %d dynsym symbols)\n", filePath, len(image), symtabCount, dynsymCount)
}

func verify(image []byte, symtabCount, dynsymCount int, verifyIndexes []int) {
	f, err := elf.NewFile(bytes.NewReader(image))
	if err != nil {
		fmt.Fprintf(os.Stderr, "elf.NewFile verification: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	symtab := f.SectionByType(elf.SHT_SYMTAB)
	dynsym := f.SectionByType(elf.SHT_DYNSYM)
	if symtab == nil || dynsym == nil {
		fmt.Fprintln(os.Stderr, "verification: symtab/dynsym section missing")
		os.Exit(1)
	}
	wantSymtabCount := uint64(symtabCount)
	wantDynsymCount := uint64(dynsymCount)
	if got := symtab.Size/elf.Sym64Size - 1; got != wantSymtabCount {
		fmt.Fprintf(os.Stderr, "verification: symtab count %d, want %d\n", got, symtabCount)
		os.Exit(1)
	}
	if got := dynsym.Size/elf.Sym64Size - 1; got != wantDynsymCount {
		fmt.Fprintf(os.Stderr, "verification: dynsym count %d, want %d\n", got, dynsymCount)
		os.Exit(1)
	}

	funcs := func(data []byte) uint64 {
		var count uint64
		for offset := elf.Sym64Size; offset < len(data); offset += elf.Sym64Size {
			if data[offset+4]&0xf == byte(elf.STT_FUNC) {
				count++
			}
		}
		return count
	}
	symtabData, err := symtab.Data()
	if err != nil {
		fmt.Fprintf(os.Stderr, "verification: symtab data: %v\n", err)
		os.Exit(1)
	}
	dynsymData, err := dynsym.Data()
	if err != nil {
		fmt.Fprintf(os.Stderr, "verification: dynsym data: %v\n", err)
		os.Exit(1)
	}
	if got := funcs(symtabData); got != wantSymtabCount*2/5 {
		fmt.Fprintf(os.Stderr, "verification: symtab funcs %d, want %d\n", got, symtabCount*2/5)
		os.Exit(1)
	}
	if got := funcs(dynsymData); got != wantDynsymCount*3/5 {
		fmt.Fprintf(os.Stderr, "verification: dynsym funcs %d, want %d\n", got, dynsymCount*3/5)
		os.Exit(1)
	}

	strtabData, err := f.Sections[symtab.Link].Data()
	if err != nil {
		fmt.Fprintf(os.Stderr, "verification: strtab data: %v\n", err)
		os.Exit(1)
	}
	for _, index := range verifyIndexes {
		nameOffset := binary.LittleEndian.Uint32(symtabData[elf.Sym64Size*(index+1) : elf.Sym64Size*(index+1)+4])
		end := bytes.IndexByte(strtabData[nameOffset:], 0)
		if got := string(strtabData[nameOffset : nameOffset+uint32(end)]); got != symName(index) {
			fmt.Fprintf(os.Stderr, "verification: symbol %d name mismatch (len %d)\n", index, len(got))
			os.Exit(1)
		}
	}
}
