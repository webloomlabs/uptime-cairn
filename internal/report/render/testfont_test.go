package render

import (
	"encoding/binary"
	"sort"
)

// A synthetic TrueType face, built here rather than checked in.
//
// # Why this exists
//
// ADR-007 item 4 requires an embedded TrueType family, and that family is a
// vendored binary asset — a licence choice and a visual identity commitment that
// belongs to the maintainer, not to a test. So the code takes a Family and the
// tests build one, which has two further benefits worth stating: the suite has
// no binary fixture in it, and the parser is exercised against a file whose
// every field is known rather than against a face whose metrics have to be
// looked up to assert anything.
//
// The face is structurally valid — real table directory, real `cmap` format 4,
// real `hmtx` — with empty outlines, because nothing in this package rasterises
// a glyph. Table checksums are zero: the reader does not verify them, and a
// checksum routine here would be testing the test.
//
// Every glyph advances 600/1000 em, so measurement is arithmetic a reader can do:
// at 10pt, a character is 6 points wide and an n-character string is 6n.
const (
	testAdvance   = 600
	testUnitsPerE = 1000
)

func testFamily() Family {
	regular, err := ParseTrueType(buildTestFont(false))
	if err != nil {
		panic(err)
	}
	bold, err := ParseTrueType(buildTestFont(true))
	if err != nil {
		panic(err)
	}
	return Family{Regular: regular, Bold: bold}
}

// The character set the face covers: printable ASCII, the Latin-1 supplement,
// and the punctuation block Compose actually reaches for — an en dash in a
// period range, an em dash in a footer, an ellipsis from a truncated table cell.
//
// The Latin-1 range is here because the suite demanded it: the burn rate renders
// as "33.33×", and U+00D7 is not in ASCII. Any Latin face covers it; a test face
// that did not was testing the wrong thing.
var testSegments = []struct{ first, last rune }{
	{0x0020, 0x007E},
	{0x00A0, 0x00FF},
	{0x2013, 0x2026},
}

var testNumGlyphs = func() int {
	n := 1
	for _, seg := range testSegments {
		n += int(seg.last-seg.first) + 1
	}
	return n
}()

func testGID(r rune) uint16 {
	gid := uint16(1)
	for _, seg := range testSegments {
		if r >= seg.first && r <= seg.last {
			return gid + uint16(r-seg.first)
		}
		gid += uint16(seg.last-seg.first) + 1
	}
	return 0
}

func testRune(gid uint16) rune {
	base := uint16(1)
	for _, seg := range testSegments {
		count := uint16(seg.last-seg.first) + 1
		if gid >= base && gid < base+count {
			return seg.first + rune(gid-base)
		}
		base += count
	}
	return 0
}

func buildTestFont(bold bool) []byte { return buildTestFontMetrics(bold, testNumGlyphs) }

// buildTestFontMetrics allows a short hmtx, where the last long metric's advance
// is shared by every glyph after it. That rule is the most-forgotten detail of
// the table and shows up as a monospaced-looking tail, so it gets its own face
// rather than being taken on trust.
func buildTestFontMetrics(bold bool, longMetrics int) []byte {
	tables := map[string][]byte{
		"OS/2": testOS2(bold),
		"cmap": testCmap(),
		"glyf": testGlyf(),
		"head": testHead(bold),
		"hhea": testHhea(longMetrics),
		"hmtx": testHmtx(longMetrics),
		"loca": testLoca(),
		"maxp": testMaxp(),
		"name": testName(bold),
		"post": make([]byte, 32),
	}

	tags := make([]string, 0, len(tables))
	for tag := range tables {
		tags = append(tags, tag)
	}
	sort.Strings(tags)

	header := 12 + len(tags)*16
	offset := header
	offsets := map[string]int{}
	for _, tag := range tags {
		offsets[tag] = offset
		offset += (len(tables[tag]) + 3) &^ 3 // four-byte aligned, as the spec requires
	}

	out := make([]byte, offset)
	binary.BigEndian.PutUint32(out[0:], 0x00010000)
	binary.BigEndian.PutUint16(out[4:], uint16(len(tags)))
	for i, tag := range tags {
		rec := 12 + i*16
		copy(out[rec:], tag)
		binary.BigEndian.PutUint32(out[rec+8:], uint32(offsets[tag]))
		binary.BigEndian.PutUint32(out[rec+12:], uint32(len(tables[tag])))
		copy(out[offsets[tag]:], tables[tag])
	}
	return out
}

// testGlyf gives every glyph a filled box.
//
// Empty outlines are enough for measurement and for text extraction, and the
// first cut of this face had them — but then a rasteriser draws nothing, and
// "nothing" is indistinguishable from a layout that put the text off the page.
// A box per character makes alignment, anchoring and page breaks visible to
// anyone who opens the output, which is the only visual check available while
// the real family is unvendored.
const testGlyphLen = 34

func testGlyf() []byte {
	glyph := make([]byte, testGlyphLen)
	putI16(glyph[0:], 1)   // one contour
	putI16(glyph[2:], 50)  // xMin
	putI16(glyph[4:], 0)   // yMin
	putI16(glyph[6:], 450) // xMax
	putI16(glyph[8:], 600) // yMax
	putI16(glyph[10:], 3)  // endPtsOfContours: four points, last index 3
	putI16(glyph[12:], 0)  // no instructions
	for i := range 4 {
		glyph[14+i] = 0x01 // on-curve, coordinates as signed 16-bit deltas
	}
	xs := []int16{50, 400, 0, -400}
	ys := []int16{0, 0, 600, 0}
	for i := range 4 {
		putI16(glyph[18+i*2:], xs[i])
		putI16(glyph[26+i*2:], ys[i])
	}

	// Glyph 0 is .notdef and stays empty: a box there would be indistinguishable
	// from a real character, and the suite asserts .notdef is never drawn.
	out := make([]byte, 0, (testNumGlyphs-1)*testGlyphLen)
	for range testNumGlyphs - 1 {
		out = append(out, glyph...)
	}
	return out
}

// testLoca is the short format: offsets divided by two, one more entry than
// there are glyphs.
func testLoca() []byte {
	b := make([]byte, (testNumGlyphs+1)*2)
	for i := range testNumGlyphs + 1 {
		offset := 0
		if i > 0 {
			offset = (i - 1) * testGlyphLen
		}
		binary.BigEndian.PutUint16(b[i*2:], uint16(offset/2))
	}
	return b
}

func testHead(bold bool) []byte {
	b := make([]byte, 54)
	binary.BigEndian.PutUint32(b[0:], 0x00010000)
	binary.BigEndian.PutUint32(b[12:], 0x5F0F3CF5) // magic
	binary.BigEndian.PutUint16(b[18:], testUnitsPerE)
	putI16(b[36:], -100) // xMin
	putI16(b[38:], -250) // yMin
	putI16(b[40:], 1000) // xMax
	putI16(b[42:], 900)  // yMax
	if bold {
		binary.BigEndian.PutUint16(b[44:], 0x0001) // macStyle bold
	}
	return b
}

func testHhea(longMetrics int) []byte {
	b := make([]byte, 36)
	binary.BigEndian.PutUint32(b[0:], 0x00010000)
	putI16(b[4:], 800)  // ascender
	putI16(b[6:], -200) // descender
	binary.BigEndian.PutUint16(b[34:], uint16(longMetrics))
	return b
}

func testHmtx(longMetrics int) []byte {
	b := make([]byte, longMetrics*4+(testNumGlyphs-longMetrics)*2)
	for i := range longMetrics {
		binary.BigEndian.PutUint16(b[i*4:], testAdvance)
	}
	return b
}

func testMaxp() []byte {
	b := make([]byte, 32)
	binary.BigEndian.PutUint32(b[0:], 0x00010000)
	binary.BigEndian.PutUint16(b[4:], uint16(testNumGlyphs))
	return b
}

func testOS2(bold bool) []byte {
	b := make([]byte, 96)
	binary.BigEndian.PutUint16(b[0:], 4) // version 4, which has sCapHeight
	weight := uint16(400)
	if bold {
		weight = 700
		binary.BigEndian.PutUint16(b[62:], 0x0020) // fsSelection BOLD
	}
	binary.BigEndian.PutUint16(b[4:], weight)
	putI16(b[68:], 800)  // sTypoAscender
	putI16(b[70:], -200) // sTypoDescender
	putI16(b[88:], 700)  // sCapHeight
	return b
}

// testCmap writes a format 4 subtable over testSegments, plus the terminating
// 0xFFFF segment the format requires.
func testCmap() []byte {
	segs := len(testSegments) + 1
	length := 16 + segs*8
	sub := make([]byte, length)
	binary.BigEndian.PutUint16(sub[0:], 4)
	binary.BigEndian.PutUint16(sub[2:], uint16(length))
	binary.BigEndian.PutUint16(sub[6:], uint16(segs*2))

	// searchRange is 2 × the largest power of two ≤ segCount; the two fields
	// after it are derived from that. Unused by any reader written this century,
	// and wrong values are the kind of thing a validator flags.
	pow := 1
	for pow*2 <= segs {
		pow *= 2
	}
	sel := 0
	for 1<<(sel+1) <= pow {
		sel++
	}
	binary.BigEndian.PutUint16(sub[8:], uint16(pow*2))
	binary.BigEndian.PutUint16(sub[10:], uint16(sel))
	binary.BigEndian.PutUint16(sub[12:], uint16(segs*2-pow*2))

	end, start, delta := 14, 14+segs*2+2, 14+segs*4+2
	for i, seg := range testSegments {
		binary.BigEndian.PutUint16(sub[end+i*2:], uint16(seg.last))
		binary.BigEndian.PutUint16(sub[start+i*2:], uint16(seg.first))
		// idDelta is a signed field that wraps: gid = (code + delta) mod 65536.
		// Computed in a wider signed type, because the constant folder will not
		// convert a negative value to uint16 on our behalf.
		binary.BigEndian.PutUint16(sub[delta+i*2:], uint16(int32(testGID(seg.first))-int32(seg.first)))
	}
	last := segs - 1
	binary.BigEndian.PutUint16(sub[end+last*2:], 0xFFFF)
	binary.BigEndian.PutUint16(sub[start+last*2:], 0xFFFF)
	binary.BigEndian.PutUint16(sub[delta+last*2:], 1)

	out := make([]byte, 4+8+len(sub))
	binary.BigEndian.PutUint16(out[2:], 1) // one encoding record
	binary.BigEndian.PutUint16(out[4:], 3) // platform 3, Windows
	binary.BigEndian.PutUint16(out[6:], 1) // encoding 1, BMP
	binary.BigEndian.PutUint32(out[8:], 12)
	copy(out[12:], sub)
	return out
}

func testName(bold bool) []byte {
	name := "CairnTest-Regular"
	if bold {
		name = "CairnTest-Bold"
	}
	utf16 := make([]byte, 0, len(name)*2)
	for _, c := range name {
		utf16 = append(utf16, 0, byte(c))
	}

	b := make([]byte, 6+12+len(utf16))
	binary.BigEndian.PutUint16(b[2:], 1)  // one record
	binary.BigEndian.PutUint16(b[4:], 18) // storage offset
	binary.BigEndian.PutUint16(b[6:], 3)  // platform 3
	binary.BigEndian.PutUint16(b[8:], 1)  // encoding 1
	binary.BigEndian.PutUint16(b[10:], 0x0409)
	binary.BigEndian.PutUint16(b[12:], 6) // nameID 6, PostScript name
	binary.BigEndian.PutUint16(b[14:], uint16(len(utf16)))
	binary.BigEndian.PutUint16(b[16:], 0)
	copy(b[18:], utf16)
	return b
}

func putI16(b []byte, v int16) { binary.BigEndian.PutUint16(b, uint16(v)) }
