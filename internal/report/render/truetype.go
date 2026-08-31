package render

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// A TrueType face reader, sufficient for embedding a face whole into a PDF and
// measuring text with it — and no more than that.
//
// What it reads: the table directory, `head`, `hhea`, `hmtx`, `maxp`, `cmap`,
// and, where present, `OS/2` and `post`. That is the complete set the PDF
// /FontDescriptor and the measurement pass need.
//
// What it deliberately does not read: `glyf` and `loca` outlines, because the
// file is embedded whole and the reader never has to draw a glyph itself; `kern`
// and `GPOS`, because kerning changes measurement and drawing together and
// applying it to only one of them is worse than applying it to neither; and
// `GSUB`, because shaping is out of scope for Phase 2 (ADR-007 item 5) and the
// Run primitive is where it arrives when it is not.
//
// Every read is bounds-checked and every error is returned rather than panicking.
// A font is a file on disk that an operator could in principle replace, and a
// malformed one must fail a report run, not the process serving it.

var errFontTruncated = errors.New("render: font file is truncated")

type ttf struct {
	raw  []byte
	name string

	unitsPerEm float64
	numGlyphs  int

	// advances is per glyph id, in font units. Indexing past the last entry is
	// legal in TrueType — a monospaced tail shares one advance — so the reader
	// expands it to numGlyphs rather than making every caller remember that.
	advances []uint16

	runeToGID map[rune]uint16

	ascent, descent, capHeight float64
	italicAngle, stemV         float64
	bbox                       [4]float64
	bold                       bool
}

// ParseTrueType reads a TrueType face.
//
// The bytes are retained rather than copied: the caller is embedding a file it
// has just loaded, and a second copy of a megabyte per face per render is a cost
// with nothing behind it. Callers must not mutate the slice afterwards, which
// is trivially satisfied by the only caller there will be — a //go:embed.
func ParseTrueType(raw []byte) (Font, error) {
	if len(raw) < 12 {
		return nil, errFontTruncated
	}

	switch tag := be32(raw, 0); tag {
	case 0x00010000, 0x74727565: // 1.0, and Apple's 'true'
	case 0x4F54544F: // 'OTTO'
		// CFF outlines. A PDF /FontFile2 stream must contain TrueType outlines,
		// so an OTTO face would produce a document that either fails to open or
		// renders nothing — refused here, by name, rather than at the reader.
		return nil, errors.New("render: OpenType/CFF font: PDF embedding needs TrueType outlines, not CFF")
	case 0x74746366: // 'ttcf'
		return nil, errors.New("render: TrueType collection: extract a single face first")
	default:
		return nil, fmt.Errorf("render: not a TrueType font (sfnt tag %#08x)", tag)
	}

	numTables := int(be16(raw, 4))
	if len(raw) < 12+numTables*16 {
		return nil, errFontTruncated
	}

	tables := make(map[string][]byte, numTables)
	for i := range numTables {
		rec := 12 + i*16
		tag := string(raw[rec : rec+4])
		off, length := int(be32(raw, rec+8)), int(be32(raw, rec+12))
		if off < 0 || length < 0 || off+length > len(raw) {
			// A table that runs off the end. Some real fonts pad the last table
			// short of its stated length; clamping rather than refusing keeps a
			// working face working, and a genuinely truncated one still fails
			// when the parse below cannot find what it needs.
			if off < 0 || off > len(raw) {
				return nil, errFontTruncated
			}
			length = len(raw) - off
		}
		tables[tag] = raw[off : off+length]
	}

	f := &ttf{raw: raw, runeToGID: map[rune]uint16{}}

	if err := f.readHead(tables["head"]); err != nil {
		return nil, err
	}
	if err := f.readMaxp(tables["maxp"]); err != nil {
		return nil, err
	}
	if err := f.readHmtx(tables["hhea"], tables["hmtx"]); err != nil {
		return nil, err
	}
	if err := f.readCmap(tables["cmap"]); err != nil {
		return nil, err
	}
	f.readOS2(tables["OS/2"])
	f.readPost(tables["post"])
	f.name = readPostScriptName(tables["name"])
	if f.name == "" {
		f.name = "CairnReportFace"
	}

	// A face with no capHeight declared: the cap height of most Latin faces is
	// close enough to 70% of the em that a descriptor built on it renders
	// correctly, and the entry is required rather than optional.
	if f.capHeight == 0 {
		f.capHeight = 700
	}
	if f.ascent == 0 {
		f.ascent = 750
	}
	if f.descent == 0 {
		f.descent = -250
	}
	// StemV has no table. Two nominal values, which is what every generator that
	// does not measure outlines does, and what the entry is used for — hinting
	// hints — makes the approximation harmless.
	f.stemV = 80
	if f.bold {
		f.stemV = 160
	}
	return f, nil
}

func (f *ttf) readHead(b []byte) error {
	if len(b) < 54 {
		return errors.New("render: font has no usable head table")
	}
	f.unitsPerEm = float64(be16(b, 18))
	if f.unitsPerEm == 0 {
		return errors.New("render: font declares unitsPerEm of zero")
	}
	f.bbox = [4]float64{
		f.scale(float64(int16(be16(b, 36)))),
		f.scale(float64(int16(be16(b, 38)))),
		f.scale(float64(int16(be16(b, 40)))),
		f.scale(float64(int16(be16(b, 42)))),
	}
	f.bold = be16(b, 44)&0x0001 != 0 // macStyle bit 0
	return nil
}

func (f *ttf) readMaxp(b []byte) error {
	if len(b) < 6 {
		return errors.New("render: font has no usable maxp table")
	}
	f.numGlyphs = int(be16(b, 4))
	if f.numGlyphs == 0 {
		return errors.New("render: font declares zero glyphs")
	}
	return nil
}

func (f *ttf) readHmtx(hhea, hmtx []byte) error {
	if len(hhea) < 36 {
		return errors.New("render: font has no usable hhea table")
	}
	f.ascent = f.scale(float64(int16(be16(hhea, 4))))
	f.descent = f.scale(float64(int16(be16(hhea, 6))))

	longMetrics := int(be16(hhea, 34))
	if longMetrics == 0 || longMetrics > f.numGlyphs {
		longMetrics = min(f.numGlyphs, max(1, longMetrics))
	}
	if len(hmtx) < longMetrics*4 {
		return errors.New("render: font hmtx table is shorter than hhea claims")
	}

	f.advances = make([]uint16, f.numGlyphs)
	var last uint16
	for i := range longMetrics {
		last = be16(hmtx, i*4)
		f.advances[i] = last
	}
	// The tail shares the final advance. Expanded here so that no caller has to
	// know the rule; it is the single most-forgotten detail of hmtx and it shows
	// up as a monospaced-looking run of the last glyphs in the face.
	for i := longMetrics; i < f.numGlyphs; i++ {
		f.advances[i] = last
	}
	return nil
}

// readCmap builds the rune-to-glyph map from the best subtable available.
//
// Preference order is (3,10) then (3,1) then (0,*): full Unicode first, the
// basic multilingual plane second, and a Unicode-platform table last. A face
// with only a (1,0) Mac Roman table is refused rather than misread — reading one
// as if it were Unicode produces a document full of the wrong glyphs, which is
// the failure that gets noticed after the report is sent.
func (f *ttf) readCmap(b []byte) error {
	if len(b) < 4 {
		return errors.New("render: font has no cmap table")
	}
	n := int(be16(b, 2))
	if len(b) < 4+n*8 {
		return errFontTruncated
	}

	best, bestScore := -1, -1
	for i := range n {
		rec := 4 + i*8
		platform, encoding := be16(b, rec), be16(b, rec+2)
		off := int(be32(b, rec+4))
		if off < 0 || off+4 > len(b) {
			continue
		}
		score := -1
		switch {
		case platform == 3 && encoding == 10:
			score = 3
		case platform == 3 && encoding == 1:
			score = 2
		case platform == 0:
			score = 1
		}
		if score > bestScore {
			best, bestScore = off, score
		}
	}
	if best < 0 {
		return errors.New("render: font has no Unicode cmap subtable")
	}

	sub := b[best:]
	switch be16(sub, 0) {
	case 4:
		return f.readCmap4(sub)
	case 12:
		return f.readCmap12(sub)
	case 6:
		return f.readCmap6(sub)
	default:
		return fmt.Errorf("render: unsupported cmap subtable format %d", be16(sub, 0))
	}
}

func (f *ttf) readCmap4(b []byte) error {
	if len(b) < 14 {
		return errFontTruncated
	}
	segCount := int(be16(b, 6)) / 2
	if segCount == 0 {
		return errors.New("render: cmap format 4 has no segments")
	}
	endAt, startAt := 14, 14+segCount*2+2
	deltaAt, rangeAt := startAt+segCount*2, startAt+segCount*4
	if len(b) < rangeAt+segCount*2 {
		return errFontTruncated
	}

	for seg := range segCount {
		end := rune(be16(b, endAt+seg*2))
		start := rune(be16(b, startAt+seg*2))
		delta := int16(be16(b, deltaAt+seg*2))
		rangeOff := int(be16(b, rangeAt+seg*2))
		if start > end {
			continue
		}
		for r := start; r <= end; r++ {
			if r == 0xFFFF {
				continue // the required terminating segment, not a character
			}
			var gid uint16
			if rangeOff == 0 {
				gid = uint16(int(r) + int(delta))
			} else {
				at := rangeAt + seg*2 + rangeOff + int(r-start)*2
				if at+2 > len(b) {
					continue
				}
				gid = be16(b, at)
				if gid != 0 {
					gid = uint16(int(gid) + int(delta))
				}
			}
			if gid != 0 && int(gid) < f.numGlyphs {
				f.runeToGID[r] = gid
			}
		}
	}
	return nil
}

func (f *ttf) readCmap6(b []byte) error {
	if len(b) < 10 {
		return errFontTruncated
	}
	first, count := rune(be16(b, 6)), int(be16(b, 8))
	if len(b) < 10+count*2 {
		return errFontTruncated
	}
	for i := range count {
		if gid := be16(b, 10+i*2); gid != 0 && int(gid) < f.numGlyphs {
			f.runeToGID[first+rune(i)] = gid
		}
	}
	return nil
}

func (f *ttf) readCmap12(b []byte) error {
	if len(b) < 16 {
		return errFontTruncated
	}
	groups := int(be32(b, 12))
	if groups < 0 || len(b) < 16+groups*12 {
		return errFontTruncated
	}
	for i := range groups {
		at := 16 + i*12
		start, end := rune(be32(b, at)), rune(be32(b, at+4))
		gid := be32(b, at+8)
		if start > end || end-start > 0x10FFFF {
			continue
		}
		for r := start; r <= end; r++ {
			g := gid + uint32(r-start)
			if g != 0 && int(g) < f.numGlyphs {
				f.runeToGID[r] = uint16(g)
			}
		}
	}
	return nil
}

// readOS2 takes the typographic metrics where the table has them. Absent, the
// hhea values already read stand — which is what a face with no OS/2 table is
// asking for.
func (f *ttf) readOS2(b []byte) {
	if len(b) < 78 {
		return
	}
	if weight := be16(b, 4); weight >= 600 {
		f.bold = true
	}
	if asc := int16(be16(b, 68)); asc != 0 {
		f.ascent = f.scale(float64(asc))
	}
	if desc := int16(be16(b, 70)); desc != 0 {
		f.descent = f.scale(float64(desc))
	}
	// fsSelection bit 5 is BOLD, and it is the field a face is most likely to be
	// honest in when macStyle is not.
	if be16(b, 62)&0x0020 != 0 {
		f.bold = true
	}
	if be16(b, 0) >= 2 && len(b) >= 90 {
		if cap := int16(be16(b, 88)); cap != 0 {
			f.capHeight = f.scale(float64(cap))
		}
	}
}

func (f *ttf) readPost(b []byte) {
	if len(b) < 8 {
		return
	}
	// italicAngle is a 16.16 fixed-point degree count.
	f.italicAngle = float64(int32(be32(b, 4))) / 65536
}

// readPostScriptName pulls name ID 6, which is what /BaseFont wants. A face with
// no usable name still renders; the entry is an identifier in the file, not
// something a reader resolves.
func readPostScriptName(b []byte) string {
	if len(b) < 6 {
		return ""
	}
	count := int(be16(b, 2))
	storage := int(be16(b, 4))
	if len(b) < 6+count*12 {
		return ""
	}
	for i := range count {
		rec := 6 + i*12
		if be16(b, rec+6) != 6 { // nameID
			continue
		}
		platform, encoding := be16(b, rec), be16(b, rec+2)
		length, off := int(be16(b, rec+8)), int(be16(b, rec+10))
		at := storage + off
		if at < 0 || at+length > len(b) {
			continue
		}
		raw := b[at : at+length]
		// Windows names are UTF-16BE; Macintosh names are single-byte. Both are
		// ASCII in practice for a PostScript name, which is all this is used for.
		if platform == 3 || (platform == 0 && encoding != 0) || platform == 0 {
			var out []byte
			for j := 0; j+1 < len(raw); j += 2 {
				if raw[j] == 0 && raw[j+1] >= 0x20 && raw[j+1] < 0x7F {
					out = append(out, raw[j+1])
				}
			}
			if len(out) > 0 {
				return string(out)
			}
		}
		var out []byte
		for _, c := range raw {
			if c >= 0x20 && c < 0x7F {
				out = append(out, c)
			}
		}
		if len(out) > 0 {
			return string(out)
		}
	}
	return ""
}

func (f *ttf) scale(v float64) float64 { return v * 1000 / f.unitsPerEm }

func (f *ttf) PostScriptName() string { return f.name }
func (f *ttf) NumGlyphs() int         { return f.numGlyphs }
func (f *ttf) Ascent() float64        { return f.ascent }
func (f *ttf) Descent() float64       { return f.descent }
func (f *ttf) CapHeight() float64     { return f.capHeight }
func (f *ttf) ItalicAngle() float64   { return f.italicAngle }
func (f *ttf) StemV() float64         { return f.stemV }
func (f *ttf) BBox() [4]float64       { return f.bbox }
func (f *ttf) Bold() bool             { return f.bold }
func (f *ttf) Raw() []byte            { return f.raw }

func (f *ttf) GlyphID(r rune) uint16 { return f.runeToGID[r] }

func (f *ttf) Advance(r rune) float64 { return f.AdvanceGID(f.runeToGID[r]) }

func (f *ttf) AdvanceGID(gid uint16) float64 {
	if int(gid) >= len(f.advances) {
		return 0
	}
	return f.scale(float64(f.advances[gid]))
}

func be16(b []byte, off int) uint16 {
	if off < 0 || off+2 > len(b) {
		return 0
	}
	return binary.BigEndian.Uint16(b[off:])
}

func be32(b []byte, off int) uint32 {
	if off < 0 || off+4 > len(b) {
		return 0
	}
	return binary.BigEndian.Uint32(b[off:])
}
