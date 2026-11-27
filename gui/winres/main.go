// winres writes an .ico as a Windows resource object (.syso) that the Go linker puts in the
// .exe: its images as RT_ICON 1..n and the group as RT_GROUP_ICON 1, the icon Gio loads for
// the window and Explorer shows for the file.
//
//	go run ./winres icon.ico rsrc_windows_amd64.syso
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: winres <in.ico> <out.syso>")
		os.Exit(2)
	}
	ico, err := os.ReadFile(os.Args[1])
	if err == nil {
		var obj []byte
		if obj, err = syso(ico); err == nil {
			err = os.WriteFile(os.Args[2], obj, 0o644)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

const (
	rtIcon      = 3
	rtGroupIcon = 14
	langEnUS    = 0x409
	// IMAGE_REL_AMD64_ADDR32NB: a data entry's offset becomes an RVA once linked
	relAddr32NB = 3
)

// syso is ico as an amd64 COFF object with one .rsrc section.
func syso(ico []byte) ([]byte, error) {
	if len(ico) < 6 || binary.LittleEndian.Uint16(ico[2:]) != 1 {
		return nil, errors.New("not an .ico")
	}
	n := int(binary.LittleEndian.Uint16(ico[4:]))
	if len(ico) < 6+16*n {
		return nil, errors.New("truncated .ico")
	}
	// the images, and the group that lists them by id instead of by offset
	var images [][]byte
	group := binary.LittleEndian.AppendUint16(nil, 0)
	group = binary.LittleEndian.AppendUint16(group, 1)
	group = binary.LittleEndian.AppendUint16(group, uint16(n))
	for i := range n {
		e := ico[6+16*i : 22+16*i]
		size, off := binary.LittleEndian.Uint32(e[8:]), binary.LittleEndian.Uint32(e[12:])
		if uint64(off)+uint64(size) > uint64(len(ico)) {
			return nil, fmt.Errorf("image %d is outside the file", i)
		}
		images = append(images, ico[off:off+size])
		group = append(group, e[:12]...)
		group = binary.LittleEndian.AppendUint16(group, uint16(i+1))
	}

	// the directory: root → type → id → language → data entry, entries sorted by id
	type leaf struct {
		typ, id int
		data    []byte
	}
	var leaves []leaf
	for i, img := range images {
		leaves = append(leaves, leaf{rtIcon, i + 1, img})
	}
	leaves = append(leaves, leaf{rtGroupIcon, 1, group})

	dirSize := func(entries int) int { return 16 + 8*entries }
	// layout: root (2 types), the RT_ICON id dir (n), the RT_GROUP_ICON id dir (1), a
	// language dir per leaf (1 each), then the data entries, then the data
	rootAt := 0
	iconDirAt := rootAt + dirSize(2)
	groupDirAt := iconDirAt + dirSize(n)
	langAt := groupDirAt + dirSize(1)
	entryAt := langAt + len(leaves)*dirSize(1)
	dataAt := entryAt + 16*len(leaves)

	var sec bytes.Buffer
	dir := func(entries ...[2]uint32) {
		sec.Write(make([]byte, 12))
		binary.Write(&sec, binary.LittleEndian, uint16(0))
		binary.Write(&sec, binary.LittleEndian, uint16(len(entries)))
		for _, e := range entries {
			binary.Write(&sec, binary.LittleEndian, e)
		}
	}
	const subdir = 1 << 31
	dir([2]uint32{rtIcon, subdir | uint32(iconDirAt)}, [2]uint32{rtGroupIcon, subdir | uint32(groupDirAt)})
	var ids [][2]uint32
	for i := range n {
		ids = append(ids, [2]uint32{uint32(i + 1), subdir | uint32(langAt+i*dirSize(1))})
	}
	dir(ids...)
	dir([2]uint32{1, subdir | uint32(langAt+n*dirSize(1))})
	for i := range leaves {
		dir([2]uint32{langEnUS, uint32(entryAt + 16*i)})
	}
	// each entry points at its data, 8-byte aligned; the offset is relocated to an RVA
	var relocs []uint32
	at := dataAt
	var data bytes.Buffer
	for _, l := range leaves {
		relocs = append(relocs, uint32(sec.Len()))
		binary.Write(&sec, binary.LittleEndian, [4]uint32{uint32(at), uint32(len(l.data)), 0, 0})
		data.Write(l.data)
		for data.Len()%8 != 0 {
			data.WriteByte(0)
		}
		at = dataAt + data.Len()
	}
	sec.Write(data.Bytes())

	// the object: file header, one section header, the section, its relocations, one symbol
	// (the section's own, which the relocations name), and an empty string table
	const headers = 20 + 40
	relocAt := headers + sec.Len()
	symAt := relocAt + 10*len(relocs)
	var obj bytes.Buffer
	binary.Write(&obj, binary.LittleEndian, struct {
		Machine, Sections        uint16
		Time, Symbols, NSymbols  uint32
		OptHeader, Characterstcs uint16
	}{0x8664, 1, 0, uint32(symAt), 1, 0, 0})
	binary.Write(&obj, binary.LittleEndian, struct {
		Name                               [8]byte
		VSize, VAddr, Size, Raw, Relocs, L uint32
		NRelocs, NLines                    uint16
		Characteristics                    uint32
	}{[8]byte{'.', 'r', 's', 'r', 'c'}, 0, 0, uint32(sec.Len()), headers, uint32(relocAt), 0, uint16(len(relocs)), 0, 0x40000040})
	obj.Write(sec.Bytes())
	for _, r := range relocs {
		binary.Write(&obj, binary.LittleEndian, r)
		binary.Write(&obj, binary.LittleEndian, uint32(0))
		binary.Write(&obj, binary.LittleEndian, uint16(relAddr32NB))
	}
	binary.Write(&obj, binary.LittleEndian, struct {
		Name             [8]byte
		Value            uint32
		Section          int16
		Type             uint16
		Class, AuxSymbls uint8
	}{[8]byte{'.', 'r', 's', 'r', 'c'}, 0, 1, 0, 3, 0})
	binary.Write(&obj, binary.LittleEndian, uint32(4))
	return obj.Bytes(), nil
}
