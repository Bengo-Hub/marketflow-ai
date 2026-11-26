package media

import (
	"bytes"
	"image"
	"image/color"
	"strings"
	"testing"
)

const testPage = "marketflow.codevertexafrica.com/s/Ab3xY7k"

// The lead page is drawn once, whole, inside the margins, in every format, and never changes a card
// without one.
func TestEndCardLeadPage(t *testing.T) {
	plain := Overlay{Headline: "Soft soap", CTA: "Order today", Contacts: []Contact{{Kind: ContactWhatsApp, Value: "0712 345 678"}}}
	with := plain
	with.Contacts = append([]Contact{}, append(plain.Contacts, Contact{Kind: ContactPage, Value: "https://" + testPage + "/"})...)
	for format, size := range Formats {
		a, err := ComposeOverlay(format, plain)
		if err != nil {
			t.Fatal(err)
		}
		b, err := ComposeOverlay(format, with)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(a.Pix, b.Pix) {
			t.Errorf("%s: no page drawn", format)
		}
		_, reg, _ := fonts()
		margin := layoutUnit(size.X, size.Y) * 6 / 100
		canvas := image.NewRGBA(image.Rect(0, 0, size.X, size.Y))
		top := drawPage(canvas, reg, testPage, margin, size.Y-margin, size.X-2*margin, layoutUnit(size.X, size.Y), color.RGBA{0x10, 0x80, 0x40, 0xff})
		if top >= size.Y-margin {
			t.Errorf("%s: the page does not fit", format)
		}
		for y := 0; y < size.Y; y++ {
			if _, _, _, al := canvas.At(size.X-2, y).RGBA(); al != 0 {
				t.Fatalf("%s: the page runs off the frame", format)
			}
		}
		if !strings.HasSuffix(overlayWords(with), testPage) {
			t.Errorf("the reviewer is not told: %s", overlayWords(with))
		}
	}
	// A card already full keeps its words and leaves the page out.
	full := with
	full.Headline, full.Price, full.Subline = strings.Repeat("A long headline ", 8), "KES 1,500", strings.Repeat("more words ", 14)
	full.CTA = strings.Repeat("Message us now ", 6)
	full.Contacts = []Contact{{Kind: ContactWhatsApp, Value: "0712 345 678"}, {Kind: ContactPhone, Value: "0799 999 999"},
		{Kind: ContactEmail, Value: "hello@a-long-business-name.example"}, {Kind: ContactWeb, Value: "a-long-business-name.example"}, {Kind: ContactPage, Value: testPage}}
	if pageFits(1920, 1080, full) {
		t.Error("a full landscape card has no room")
	}
	a, _ := ComposeOverlay("1920x1080", full)
	b, _ := ComposeOverlay("1920x1080", full.withoutPage())
	if !bytes.Equal(a.Pix, b.Pix) {
		t.Error("the page was drawn over a full card")
	}
	// Never twice, never as a plain contact, and an unreadable one not at all.
	for _, list := range [][]Contact{
		{{Kind: ContactWeb, Value: "www." + testPage}, {Kind: ContactPage, Value: testPage}},
		{{Kind: ContactPage, Value: strings.Repeat("x", 61)}},
	} {
		if got := pageOf(list); got != "" {
			t.Errorf("%q", got)
		}
	}
	if len(CleanContacts(with.Contacts)) != 1 {
		t.Error("the page is not a contact")
	}
}
