package api

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
)

func brandClient(t *testing.T) *client {
	t.Helper()
	server, _, _ := testAPI(t)
	c := newClient(t, server)
	c.setup()
	return c
}

func createBrand(t *testing.T, c *client, body map[string]any) string {
	t.Helper()
	resp, out := c.do(http.MethodPost, "/api/v1/brand-profiles", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create brand profile = %d, want 201 (%v)", resp.StatusCode, out)
	}
	return out["id"].(string)
}

func pngLogo(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 48, 16))
	for y := range 16 {
		for x := range 48 {
			img.Set(x, y, color.NRGBA{R: 0x1a, G: 0x8f, B: 0x5a, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// upload PUTs raw bytes, which `do` cannot: it encodes JSON.
func (c *client) upload(t *testing.T, path, contentType string, body []byte) (*http.Response, map[string]any) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, c.base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	c.authorise(req)

	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	var decoded map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return resp, decoded
}

// A profile round-trips, and the colour comes back exactly as it was written.
// A brand colour is a string pasted from a brand guide; handing it back in a
// different case is the kind of small wrongness that makes a white-label
// feature feel like somebody else's.
func TestBrandProfileRoundTripsThroughTheAPI(t *testing.T) {
	t.Parallel()

	c := brandClient(t)
	id := createBrand(t, c, map[string]any{
		"name": "Acme", "company_name": "Smith & Co",
		"primary_color": "#1A8F5A", "accent_color": "#3b5bdb",
		"footer_text": "Confidential.", "cover_text": "Monthly availability summary.",
		"is_default": true,
	})

	_, body := c.do(http.MethodGet, "/api/v1/brand-profiles/"+id, nil)
	if body["primary_color"] != "#1A8F5A" {
		t.Errorf("primary_color = %v, want it unchanged", body["primary_color"])
	}
	if body["is_default"] != true {
		t.Error("the default flag was lost")
	}
	if body["logo_content_type"] != nil {
		t.Errorf("a profile with no logo reports a content type: %v", body["logo_content_type"])
	}
}

// A colour that is not a six-digit hex is refused at the field, with a message
// naming the shape rather than restating the pattern.
func TestABadColourIsRefusedAtItsField(t *testing.T) {
	t.Parallel()

	c := brandClient(t)
	resp, body := c.do(http.MethodPost, "/api/v1/brand-profiles", map[string]any{
		"name": "Acme", "primary_color": "green",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%v)", resp.StatusCode, body)
	}
	item := body["errors"].([]any)[0].(map[string]any)
	if item["pointer"] != "/primary_color" {
		t.Errorf("pointer = %v", item["pointer"])
	}
	if msg, _ := item["message"].(string); !strings.Contains(msg, "#1a8f5a") {
		t.Errorf("message = %q, want an example of the right shape", msg)
	}
}

// Marking a profile default demotes the previous one, so the unique index never
// has to refuse a write an operator meant.
func TestMarkingADefaultDemotesThePreviousOne(t *testing.T) {
	t.Parallel()

	c := brandClient(t)
	first := createBrand(t, c, map[string]any{"name": "First", "is_default": true})
	createBrand(t, c, map[string]any{"name": "Second", "is_default": true})

	_, body := c.do(http.MethodGet, "/api/v1/brand-profiles/"+first, nil)
	if body["is_default"] != false {
		t.Error("the previous default is still marked default")
	}
}

// A PNG logo uploads and is reported on the profile.
func TestAPNGLogoUploads(t *testing.T) {
	t.Parallel()

	c := brandClient(t)
	id := createBrand(t, c, map[string]any{"name": "Acme"})

	resp, body := c.upload(t, "/api/v1/brand-profiles/"+id+"/logo", "image/png", pngLogo(t))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload = %d, want 200 (%v)", resp.StatusCode, body)
	}
	if body["logo_content_type"] != "image/png" {
		t.Errorf("logo_content_type = %v", body["logo_content_type"])
	}
}

// JPEG too, decided from the bytes rather than the declared type.
func TestAJPEGLogoUploads(t *testing.T) {
	t.Parallel()

	c := brandClient(t)
	id := createBrand(t, c, map[string]any{"name": "Acme"})

	var raw bytes.Buffer
	if err := jpeg.Encode(&raw, image.NewRGBA(image.Rect(0, 0, 32, 12)), nil); err != nil {
		t.Fatal(err)
	}

	// Declared as PNG on purpose: the decision is made from the bytes, so a
	// wrong label does not produce a wrong stored content type — which would
	// send a JPEG to the PDF writer labelled as a PNG.
	resp, body := c.upload(t, "/api/v1/brand-profiles/"+id+"/logo", "image/png", raw.Bytes())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload = %d (%v)", resp.StatusCode, body)
	}
	if body["logo_content_type"] != "image/jpeg" {
		t.Errorf("logo_content_type = %v, want image/jpeg from the bytes", body["logo_content_type"])
	}
}

// **An SVG is refused at upload, by name, with the reason and the fix.** ADR-007
// calls this the single most likely source of a month-six surprise: the PDF
// writer embeds rasters and cannot draw SVG paths, and SVG is the expected case
// rather than the exotic one — status pages take an arbitrary logo URL and the
// project's own mark is an SVG. Dropping it silently at render time means an
// agency discovers it in a client's inbox.
func TestAnSVGLogoIsRefusedWithTheReasonAndTheFix(t *testing.T) {
	t.Parallel()

	c := brandClient(t)
	id := createBrand(t, c, map[string]any{"name": "Acme"})

	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="48" height="16"><rect width="48" height="16"/></svg>`)
	resp, body := c.upload(t, "/api/v1/brand-profiles/"+id+"/logo", "image/png", svg)

	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415 (%v)", resp.StatusCode, body)
	}
	detail, _ := body["detail"].(string)
	for _, want := range []string{"SVG", "PNG"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q — the operator is holding an SVG and needs to be told what to do with it", detail, want)
		}
	}

	// And nothing was stored, so the profile is not left claiming a logo it has
	// no bytes for.
	_, profile := c.do(http.MethodGet, "/api/v1/brand-profiles/"+id, nil)
	if profile["logo_content_type"] != nil {
		t.Error("a refused upload set a content type")
	}
}

// Anything else is refused too, without pretending to know what it was.
func TestAnUnknownLogoFormatIsRefused(t *testing.T) {
	t.Parallel()

	c := brandClient(t)
	id := createBrand(t, c, map[string]any{"name": "Acme"})

	resp, _ := c.upload(t, "/api/v1/brand-profiles/"+id+"/logo", "image/png", []byte("GIF89a and then some"))
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", resp.StatusCode)
	}
}

// A logo over the cap is refused rather than truncated into a corrupt image.
func TestAnOversizedLogoIsRefused(t *testing.T) {
	t.Parallel()

	c := brandClient(t)
	id := createBrand(t, c, map[string]any{"name": "Acme"})

	oversized := append(pngLogo(t), bytes.Repeat([]byte{0}, maxLogoBytes)...)
	resp, _ := c.upload(t, "/api/v1/brand-profiles/"+id+"/logo", "image/png", oversized)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

// **Deleting a profile in use is refused, and the refusal says how many
// templates to unpick.** The foreign key would allow it and let the template
// fall back to the default — invisible until an agency's client receives an
// unbranded document.
func TestDeletingAProfileInUseIsRefusedWithACount(t *testing.T) {
	t.Parallel()

	c := brandClient(t)
	brand := createBrand(t, c, map[string]any{"name": "Acme"})
	createTemplate(t, c, map[string]any{
		"name": "Monthly", "type": "uptime", "formats": []string{"json"},
		"brand_profile_id": brand,
	})

	resp, body := c.do(http.MethodDelete, "/api/v1/brand-profiles/"+brand, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete = %d, want 409 (%v)", resp.StatusCode, body)
	}
	if detail, _ := body["detail"].(string); !strings.Contains(detail, "1 report template") {
		t.Errorf("detail = %q, want it to count what is in the way", detail)
	}

	// Still there.
	if resp, _ := c.do(http.MethodGet, "/api/v1/brand-profiles/"+brand, nil); resp.StatusCode != http.StatusOK {
		t.Error("the profile was deleted despite the refusal")
	}
}

// Once nothing references it, the delete goes through.
func TestAnUnusedProfileDeletes(t *testing.T) {
	t.Parallel()

	c := brandClient(t)
	brand := createBrand(t, c, map[string]any{"name": "Acme"})

	resp, body := c.do(http.MethodDelete, "/api/v1/brand-profiles/"+brand, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204 (%v)", resp.StatusCode, body)
	}
	if resp, _ := c.do(http.MethodGet, "/api/v1/brand-profiles/"+brand, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", resp.StatusCode)
	}
}
