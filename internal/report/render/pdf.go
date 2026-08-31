package render

import (
	"bytes"
	"compress/zlib"
	"crypto/md5"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"sort"
	"strings"
	"time"
)

// The PDF content-stream backend: the second of ADR-007's two backends, and the
// reason the primitive set in draw.go is five operations rather than fifteen.
//
// It writes PDF 1.7 by hand. There is no third-party library here and there is
// not going to be one in Phase 2 — the ADR settles that on packaging grounds
// (CGO_ENABLED=0 across five targets, a single-file tarball, an image size
// already argued at eight-megabyte granularity), not on preference.
//
// # Coordinates
//
// The primitives are top-left with y down, because that is how a page is laid
// out and how SVG works. PDF's space is bottom-left with y up. The conversion is
// a subtraction and it happens **here, at each emission**, rather than through a
// page-level transformation matrix — a flipping CTM would mirror every glyph and
// then need a second flip inside every text object, which is two conversions to
// keep straight instead of one.
//
// # Determinism (ADR-007 item 6)
//
// Nothing in this file reads a clock or a random source. The creation date and
// the /ID array are derived from the report run, passed in. Object numbers are
// assigned in emission order, resources are sorted before they are written, and
// coordinates go through the same `num` formatter the SVG backend uses. The one
// place where byte-identity depends on something outside this package is image
// compression: flate output is deterministic for a given build, which satisfies
// "the same model rendered twice", and a future Go release changing its encoder
// would change stored artifacts' bytes but not their content.

// PDF is a document being drawn. It implements Backend for whichever page is
// current, so charts written against the primitives draw into it unmodified.
type PDF struct {
	family Family

	pages []*pdfPage
	cur   *pdfPage

	// usedGlyphs is per weight, across the whole document: the /W array only has
	// to carry widths for glyphs that appear, and a whole-face array for a font
	// with fifty thousand glyphs is larger than the report.
	usedGlyphs map[Weight]map[uint16]bool

	images    []*pdfImage
	imageByID map[string]*pdfImage

	err error
}

type pdfPage struct {
	width, height float64
	content       bytes.Buffer
	fonts         map[Weight]bool
	images        map[string]bool
}

type pdfImage struct {
	name   string
	key    string
	width  int
	height int
	data   []byte

	// jpeg passes the original bytes through /DCTDecode; anything else has been
	// decoded to 8-bit RGB and is flate-compressed.
	jpeg bool

	// smask holds the alpha channel where the source had one. A logo with a
	// transparent background composited onto white would be a white box on a
	// coloured cover, so the alpha is carried rather than flattened.
	smask []byte
}

// NewPDF starts a document drawn with the given family.
func NewPDF(family Family) *PDF {
	return &PDF{
		family:     family,
		usedGlyphs: map[Weight]map[uint16]bool{},
		imageByID:  map[string]*pdfImage{},
	}
}

// NewPage appends a page and makes it current. Sizes are in points.
func (p *PDF) NewPage(width, height float64) {
	page := &pdfPage{width: width, height: height, fonts: map[Weight]bool{}, images: map[string]bool{}}
	p.pages = append(p.pages, page)
	p.cur = page
}

// PageCount is how many pages have been started, which the layout pass needs for
// "page 2 of 5" and a test needs to assert a table actually broke.
func (p *PDF) PageCount() int { return len(p.pages) }

// selectPage makes an already-started page current again.
//
// It exists for one job: the running footer, which says "page 2 of 5" and
// therefore cannot be written until the last page exists. Drawing it in a second
// pass is cheaper and far less error-prone than laying the document out twice to
// discover a count, and a page's content stream does not care that its footer
// was appended after its body.
func (p *PDF) selectPage(i int) {
	if i >= 0 && i < len(p.pages) {
		p.cur = p.pages[i]
	}
}

func (p *PDF) fail(err error) {
	if p.err == nil {
		p.err = err
	}
}

// --- Backend ---------------------------------------------------------------

func (p *PDF) Text(x, y float64, run Run, style TextStyle) {
	if p.cur == nil || run.Text == "" {
		return
	}
	face := p.family.Face(style.Weight)
	if face == nil {
		p.fail(errors.New("render: no font face for the requested weight"))
		return
	}

	switch style.Anchor {
	case Middle:
		x -= Measure(face, run.Text, style.SizePt) / 2
	case End:
		x -= Measure(face, run.Text, style.SizePt)
	}

	glyphs := p.usedGlyphs[style.Weight]
	if glyphs == nil {
		glyphs = map[uint16]bool{}
		p.usedGlyphs[style.Weight] = glyphs
	}

	var hex strings.Builder
	for _, r := range run.Text {
		gid := face.GlyphID(r)
		glyphs[gid] = true
		fmt.Fprintf(&hex, "%04X", gid)
	}

	p.cur.fonts[style.Weight] = true
	fmt.Fprintf(&p.cur.content, "BT %s /%s %s Tf 1 0 0 1 %s %s Tm <%s> Tj ET\n",
		fillColor(style.Fill), fontName(style.Weight), num(style.SizePt),
		num(x), num(p.cur.height-y), hex.String())
}

func (p *PDF) Rect(r Rect, style ShapeStyle) {
	if p.cur == nil || r.W <= 0 || r.H <= 0 {
		return
	}
	bottom := p.cur.height - (r.Y + r.H)

	if style.Radius > 0 {
		p.roundedRect(r.X, bottom, r.W, r.H, min(style.Radius, min(r.W, r.H)/2), style)
		return
	}
	p.paint(fmt.Sprintf("%s %s %s %s re", num(r.X), num(bottom), num(r.W), num(r.H)), true, style)
}

// roundedRect draws the four corners as Bézier arcs. The magic constant is the
// standard circle approximation: a quarter arc of radius r is drawn with control
// points 0.5523·r along the tangents, which is accurate to about one part in a
// thousand and invisible at report scale.
func (p *PDF) roundedRect(x, y, w, h, r float64, style ShapeStyle) {
	const k = 0.5522847498
	c := r * k
	var d strings.Builder
	fmt.Fprintf(&d, "%s %s m ", num(x+r), num(y))
	fmt.Fprintf(&d, "%s %s l ", num(x+w-r), num(y))
	fmt.Fprintf(&d, "%s %s %s %s %s %s c ", num(x+w-r+c), num(y), num(x+w), num(y+r-c), num(x+w), num(y+r))
	fmt.Fprintf(&d, "%s %s l ", num(x+w), num(y+h-r))
	fmt.Fprintf(&d, "%s %s %s %s %s %s c ", num(x+w), num(y+h-r+c), num(x+w-r+c), num(y+h), num(x+w-r), num(y+h))
	fmt.Fprintf(&d, "%s %s l ", num(x+r), num(y+h))
	fmt.Fprintf(&d, "%s %s %s %s %s %s c ", num(x+r-c), num(y+h), num(x), num(y+h-r+c), num(x), num(y+h-r))
	fmt.Fprintf(&d, "%s %s l ", num(x), num(y+r))
	fmt.Fprintf(&d, "%s %s %s %s %s %s c", num(x), num(y+r-c), num(x+r-c), num(y), num(x+r), num(y))
	p.paint(d.String(), true, style)
}

func (p *PDF) Line(x1, y1, x2, y2 float64, style StrokeStyle) {
	if p.cur == nil {
		return
	}
	h := p.cur.height
	fmt.Fprintf(&p.cur.content, "%s %s w %s %s m %s %s l S\n",
		strokeColor(style.Color), num(style.Width),
		num(x1), num(h-y1), num(x2), num(h-y2))
}

func (p *PDF) Path(points []Point, closed bool, style ShapeStyle) {
	if p.cur == nil || len(points) < 2 {
		// One point is not a line — the same answer the SVG backend gives, and
		// for the same reason: a monitor with a single day of history is a real
		// case rather than a failure.
		return
	}
	h := p.cur.height
	var d strings.Builder
	for i, pt := range points {
		op := "l"
		if i == 0 {
			op = "m"
		}
		fmt.Fprintf(&d, "%s %s %s ", num(pt.X), num(h-pt.Y), op)
	}
	if closed {
		d.WriteString("h ")
	}
	p.paint(strings.TrimSpace(d.String()), closed, style)
}

// paint writes a path construction followed by the right painting operator.
//
// An unclosed stroked path must not be closed by the operator, which is why the
// caller says whether it was: `f` on an open polyline silently closes it, and a
// latency line that joins its last point back to its first is a chart that says
// something untrue about the data.
func (p *PDF) paint(construct string, fillable bool, style ShapeStyle) {
	var prefix strings.Builder
	if style.Fill != nil {
		prefix.WriteString(fillColor(*style.Fill) + " ")
	}
	if style.Stroke != nil {
		prefix.WriteString(strokeColor(*style.Stroke) + " " + num(style.StrokeWidth) + " w ")
	}

	op := "n" // constructed and painted with nothing: a caller's bug, not a crash
	switch {
	case style.Fill != nil && style.Stroke != nil && fillable:
		op = "B"
	case style.Fill != nil && fillable:
		op = "f"
	case style.Stroke != nil:
		op = "S"
	case style.Fill != nil:
		op = "f"
	}
	fmt.Fprintf(&p.cur.content, "%s%s %s\n", prefix.String(), construct, op)
}

func (p *PDF) Image(r Rect, mime string, data []byte) {
	if p.cur == nil || len(data) == 0 || r.W <= 0 || r.H <= 0 {
		return
	}

	key := fmt.Sprintf("%x", md5.Sum(data))
	img, ok := p.imageByID[key]
	if !ok {
		var err error
		img, err = decodeImage(key, len(p.images), mime, data)
		if err != nil {
			// A logo that cannot be decoded does not fail the report. ADR-007
			// item 7 is about formats, but the same judgement applies inside
			// one: a client would rather have an unbranded report than none.
			return
		}
		p.images = append(p.images, img)
		p.imageByID[key] = img
	}
	p.cur.images[img.name] = true

	// An image XObject draws into the unit square, so the placement is entirely
	// in the transformation matrix. Aspect ratio is preserved by fitting inside
	// the rectangle and centring, which is what `preserveAspectRatio="xMidYMid
	// meet"` does on the SVG side — the two backends have to agree about a
	// logo's shape or the same brand looks stretched in one of them.
	w, h := fitInside(float64(img.width), float64(img.height), r.W, r.H)
	x := r.X + (r.W-w)/2
	y := p.cur.height - (r.Y + (r.H-h)/2 + h)

	fmt.Fprintf(&p.cur.content, "q %s 0 0 %s %s %s cm /%s Do Q\n",
		num(w), num(h), num(x), num(y), img.name)
}

func fitInside(w, h, maxW, maxH float64) (float64, float64) {
	if w <= 0 || h <= 0 {
		return maxW, maxH
	}
	scale := min(maxW/w, maxH/h)
	return w * scale, h * scale
}

// --- image decoding --------------------------------------------------------

func decodeImage(key string, index int, mime string, data []byte) (*pdfImage, error) {
	name := fmt.Sprintf("Im%d", index)

	// JPEG passes through untouched: /DCTDecode is a PDF filter, so re-encoding
	// would cost quality and bytes to arrive at the same picture.
	if mime == "image/jpeg" || mime == "image/jpg" {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return &pdfImage{name: name, key: key, width: cfg.Width, height: cfg.Height, data: data, jpeg: true}, nil
	}

	src, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	rgb := make([]byte, 0, b.Dx()*b.Dy()*3)
	alpha := make([]byte, 0, b.Dx()*b.Dy())
	opaque := true
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := src.At(x, y).RGBA()
			// Un-premultiply, because Go's colour model is premultiplied and PDF
			// composites with a separate soft mask. A premultiplied logo drawn
			// through an SMask comes out darkened at every soft edge.
			if a > 0 && a < 0xffff {
				r = r * 0xffff / a
				g = g * 0xffff / a
				bl = bl * 0xffff / a
			}
			rgb = append(rgb, byte(min(r, 0xffff)>>8), byte(min(g, 0xffff)>>8), byte(min(bl, 0xffff)>>8))
			alpha = append(alpha, byte(a>>8))
			if a>>8 != 0xff {
				opaque = false
			}
		}
	}
	img := &pdfImage{name: name, key: key, width: b.Dx(), height: b.Dy(), data: deflate(rgb)}
	if !opaque {
		img.smask = deflate(alpha)
	}
	return img, nil
}

func deflate(b []byte) []byte {
	var out bytes.Buffer
	w, _ := zlib.NewWriterLevel(&out, zlib.BestCompression)
	w.Write(b)
	w.Close()
	return out.Bytes()
}

// --- document assembly -----------------------------------------------------

// Bytes serialises the document.
//
// runID and created come from the report run rather than from the clock, which
// is ADR-007 item 6 and the reason the plan's "a definition plus a window plus a
// data snapshot yields the same artifact" is a property rather than an
// aspiration.
func (p *PDF) Bytes(runID string, created time.Time) ([]byte, error) {
	if p.err != nil {
		return nil, p.err
	}
	if len(p.pages) == 0 {
		return nil, errors.New("render: no pages")
	}
	if p.family.Regular == nil {
		return nil, errors.New("render: no regular font face")
	}

	w := &pdfWriter{}
	w.begin()

	// Object numbers are reserved before anything is written so that the page
	// tree and its kids can refer to each other. Order is fixed rather than
	// discovered, because an object graph emitted in map order is an object
	// graph that changes between runs.
	catalog := w.reserve()
	pagesObj := w.reserve()
	pageObjs := make([]int, len(p.pages))
	contentObjs := make([]int, len(p.pages))
	for i := range p.pages {
		pageObjs[i] = w.reserve()
		contentObjs[i] = w.reserve()
	}

	fontObjs := map[Weight]int{}
	for _, weight := range []Weight{Regular, Bold} {
		if len(p.usedGlyphs[weight]) == 0 {
			continue
		}
		face := p.family.Face(weight)
		if face == nil {
			continue
		}
		fontObjs[weight] = w.writeFont(face, p.usedGlyphs[weight])
	}

	imageObjs := map[string]int{}
	for _, img := range p.images {
		imageObjs[img.name] = w.writeImage(img)
	}

	w.object(catalog, "<< /Type /Catalog /Pages "+ref(pagesObj)+" >>")

	var kids strings.Builder
	kids.WriteString("[")
	for i, obj := range pageObjs {
		if i > 0 {
			kids.WriteString(" ")
		}
		kids.WriteString(ref(obj))
	}
	kids.WriteString("]")
	w.object(pagesObj, fmt.Sprintf("<< /Type /Pages /Count %d /Kids %s >>", len(pageObjs), kids.String()))

	for i, page := range p.pages {
		var res strings.Builder
		res.WriteString("<< /ProcSet [/PDF /Text /ImageC]")

		if len(page.fonts) > 0 {
			res.WriteString(" /Font <<")
			for _, weight := range []Weight{Regular, Bold} {
				if page.fonts[weight] {
					if obj, ok := fontObjs[weight]; ok {
						res.WriteString(" /" + fontName(weight) + " " + ref(obj))
					}
				}
			}
			res.WriteString(" >>")
		}
		if len(page.images) > 0 {
			names := make([]string, 0, len(page.images))
			for name := range page.images {
				names = append(names, name)
			}
			sort.Strings(names)
			res.WriteString(" /XObject <<")
			for _, name := range names {
				res.WriteString(" /" + name + " " + ref(imageObjs[name]))
			}
			res.WriteString(" >>")
		}
		res.WriteString(" >>")

		w.object(pageObjs[i], fmt.Sprintf(
			"<< /Type /Page /Parent %s /MediaBox [0 0 %s %s] /Resources %s /Contents %s >>",
			ref(pagesObj), num(page.width), num(page.height), res.String(), ref(contentObjs[i])))

		w.stream(contentObjs[i], "", page.content.Bytes())
	}

	info := w.reserve()
	w.object(info, fmt.Sprintf("<< /Producer (Uptime Cairn) /Creator (Uptime Cairn) /CreationDate (%s) /ModDate (%s) >>",
		pdfDate(created), pdfDate(created)))

	// The /ID array identifies the file. Both halves are derived from the run so
	// that regenerating a report produces the same file rather than a file that
	// differs only where a reader cannot see.
	id := fmt.Sprintf("%X", md5.Sum([]byte(runID+"|"+created.UTC().Format(time.RFC3339))))
	w.finish(catalog, info, id)
	return w.buf.Bytes(), nil
}

type pdfWriter struct {
	buf     bytes.Buffer
	offsets []int // 1-based object number to byte offset; index 0 unused
}

func (w *pdfWriter) begin() {
	w.offsets = []int{0}
	// The binary comment tells anything sniffing the file that it is not text,
	// which matters for the tools between here and a client's inbox.
	w.buf.WriteString("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n")
}

func (w *pdfWriter) reserve() int {
	w.offsets = append(w.offsets, 0)
	return len(w.offsets) - 1
}

func (w *pdfWriter) object(num int, body string) {
	w.offsets[num] = w.buf.Len()
	fmt.Fprintf(&w.buf, "%d 0 obj\n%s\nendobj\n", num, body)
}

func (w *pdfWriter) stream(num int, extra string, data []byte) {
	w.offsets[num] = w.buf.Len()
	fmt.Fprintf(&w.buf, "%d 0 obj\n<< /Length %d%s >>\nstream\n", num, len(data), extra)
	w.buf.Write(data)
	w.buf.WriteString("\nendstream\nendobj\n")
}

func (w *pdfWriter) newStream(extra string, data []byte) int {
	num := w.reserve()
	w.stream(num, extra, data)
	return num
}

func (w *pdfWriter) finish(catalog, info int, id string) {
	start := w.buf.Len()
	fmt.Fprintf(&w.buf, "xref\n0 %d\n", len(w.offsets))
	w.buf.WriteString("0000000000 65535 f \n")
	for _, off := range w.offsets[1:] {
		fmt.Fprintf(&w.buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&w.buf, "trailer\n<< /Size %d /Root %s /Info %s /ID [<%s> <%s>] >>\nstartxref\n%d\n%%%%EOF\n",
		len(w.offsets), ref(catalog), ref(info), id, id, start)
}

// writeFont emits a Type0 font with a CIDFontType2 descendant.
//
// Type0/Identity-H rather than a simple /TrueType font, and that is the direct
// consequence of ADR-007 item 4 rejecting the base fourteen: a simple font locks
// the encoding to a single byte per character, which is the WinAnsi limitation
// the ADR names. Identity-H addresses glyphs directly, so the encoding imposes
// no character-set limit at all and the shaping layer of item 5 has somewhere to
// arrive.
func (w *pdfWriter) writeFont(face Font, used map[uint16]bool) int {
	fontFile := w.newStream(fmt.Sprintf(" /Length1 %d", len(face.Raw())), face.Raw())

	flags := 4 // symbolic: the face is addressed by glyph id, not by a standard encoding
	if face.ItalicAngle() != 0 {
		flags |= 64
	}
	bbox := face.BBox()
	descriptor := w.reserve()
	w.object(descriptor, fmt.Sprintf(
		"<< /Type /FontDescriptor /FontName /%s /Flags %d /FontBBox [%s %s %s %s]"+
			" /ItalicAngle %s /Ascent %s /Descent %s /CapHeight %s /StemV %s /FontFile2 %s >>",
		face.PostScriptName(), flags,
		num(bbox[0]), num(bbox[1]), num(bbox[2]), num(bbox[3]),
		num(face.ItalicAngle()), num(face.Ascent()), num(face.Descent()),
		num(face.CapHeight()), num(face.StemV()), ref(fontFile)))

	descendant := w.reserve()
	toUnicode := w.newStream("", buildToUnicode(face, used))

	w.object(descendant, fmt.Sprintf(
		"<< /Type /Font /Subtype /CIDFontType2 /BaseFont /%s"+
			" /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >>"+
			" /FontDescriptor %s /DW 0 /W %s /CIDToGIDMap /Identity >>",
		face.PostScriptName(), ref(descriptor), widthsArray(face, used)))

	obj := w.reserve()
	w.object(obj, fmt.Sprintf(
		"<< /Type /Font /Subtype /Type0 /BaseFont /%s /Encoding /Identity-H /DescendantFonts [%s] /ToUnicode %s >>",
		face.PostScriptName(), ref(descendant), ref(toUnicode)))
	return obj
}

// widthsArray writes /W in the run-length form the spec allows, sorted so that
// the same document produces the same array every time.
func widthsArray(face Font, used map[uint16]bool) string {
	gids := make([]int, 0, len(used))
	for gid := range used {
		gids = append(gids, int(gid))
	}
	sort.Ints(gids)

	var out strings.Builder
	out.WriteString("[")
	for i := 0; i < len(gids); {
		j := i
		for j+1 < len(gids) && gids[j+1] == gids[j]+1 {
			j++
		}
		if i > 0 {
			out.WriteString(" ")
		}
		fmt.Fprintf(&out, "%d [", gids[i])
		for k := i; k <= j; k++ {
			if k > i {
				out.WriteString(" ")
			}
			out.WriteString(num(face.AdvanceGID(uint16(gids[k]))))
		}
		out.WriteString("]")
		i = j + 1
	}
	out.WriteString("]")
	return out.String()
}

// buildToUnicode makes the document's text extractable.
//
// Without it, Identity-H glyph ids are opaque: a report copied out of a PDF
// reader comes back as mojibake, and an auditor checking a figure against a
// spreadsheet cannot paste it. That is a real use of these artifacts, so the
// CMap is written rather than treated as optional metadata.
func buildToUnicode(face Font, used map[uint16]bool) []byte {
	// The reverse map is built from the face rather than from the drawn text,
	// which means one lookup pass over the runes that were actually used.
	reverse := map[uint16]rune{}
	for r := rune(0x20); r <= 0xFFFD; r++ {
		gid := face.GlyphID(r)
		if gid != 0 && used[gid] {
			if _, seen := reverse[gid]; !seen {
				reverse[gid] = r
			}
		}
	}

	gids := make([]int, 0, len(reverse))
	for gid := range reverse {
		gids = append(gids, int(gid))
	}
	sort.Ints(gids)

	var b bytes.Buffer
	b.WriteString("/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n")
	b.WriteString("/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def\n")
	b.WriteString("/CMapName /Adobe-Identity-UCS def\n/CMapType 2 def\n")
	b.WriteString("1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n")

	// The spec caps a bfchar block at 100 entries.
	for start := 0; start < len(gids); start += 100 {
		end := min(start+100, len(gids))
		fmt.Fprintf(&b, "%d beginbfchar\n", end-start)
		for _, gid := range gids[start:end] {
			fmt.Fprintf(&b, "<%04X> <%s>\n", gid, utf16Hex(reverse[uint16(gid)]))
		}
		b.WriteString("endbfchar\n")
	}
	b.WriteString("endcmap\nCMapName currentdict /CMap defineresource pop\nend\nend\n")
	return b.Bytes()
}

func utf16Hex(r rune) string {
	if r < 0x10000 {
		return fmt.Sprintf("%04X", r)
	}
	r -= 0x10000
	return fmt.Sprintf("%04X%04X", 0xD800+(r>>10), 0xDC00+(r&0x3FF))
}

func (w *pdfWriter) writeImage(img *pdfImage) int {
	filter := " /Filter /FlateDecode"
	if img.jpeg {
		filter = " /Filter /DCTDecode"
	}

	smaskRef := ""
	if img.smask != nil {
		obj := w.newStream(fmt.Sprintf(
			" /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceGray /BitsPerComponent 8 /Filter /FlateDecode",
			img.width, img.height), img.smask)
		smaskRef = " /SMask " + ref(obj)
	}

	return w.newStream(fmt.Sprintf(
		" /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8%s%s",
		img.width, img.height, filter, smaskRef), img.data)
}

func ref(n int) string { return fmt.Sprintf("%d 0 R", n) }

func fontName(w Weight) string {
	if w == Bold {
		return "FB"
	}
	return "FR"
}

// PDF colour operands are 0–1 rather than 0–255, formatted through the same
// rounding the coordinates use so that determinism does not depend on float
// printing.
func fillColor(c Color) string {
	return fmt.Sprintf("%s %s %s rg", num(chan255(c.R)), num(chan255(c.G)), num(chan255(c.B)))
}

func strokeColor(c Color) string {
	return fmt.Sprintf("%s %s %s RG", num(chan255(c.R)), num(chan255(c.G)), num(chan255(c.B)))
}

func chan255(v uint8) float64 { return float64(v) / 255 }

// pdfDate is the D:YYYYMMDDHHmmSSOHH'mm' form. Always written in UTC, because
// the alternative is a local offset that varies with the machine that ran the
// report and would make two runs of the same definition differ.
func pdfDate(t time.Time) string {
	return "D:" + t.UTC().Format("20060102150405") + "Z00'00'"
}
