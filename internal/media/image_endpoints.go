package media

import (
	"fmt"
	"regexp"
	"strings"
)

// ImageEndpoints are the provider endpoint ids the poster and storyboard routes call, kept in
// config like the music model (AudioConfigFromEnv) so a provider renaming a model is a config
// change, not a release. Route keys and prices stay in PosterRoutes and StillRoutes.
type ImageEndpoints struct {
	SeedreamText      string // fal, text to image
	SeedreamEdit      string // fal, with the offering's photos
	NanoBanana2       string
	NanoBanana2Edit   string
	NanoBananaPro     string
	NanoBananaProEdit string
	RecraftText       string // Higgsfield
	SoulStill         string // Higgsfield, storyboard stills
}

// DefaultImageEndpoints are the ids the providers list. Seedream v5 Lite is
// "bytedance/seedream/v5/lite/..." on fal, without the "fal-ai/" prefix the other fal models carry
// (checked against fal's model list 2026-11-18); the prefixed id did not exist, so the first poster
// route failed and every poster fell back to the next one.
func DefaultImageEndpoints() ImageEndpoints {
	return ImageEndpoints{
		SeedreamText:      "bytedance/seedream/v5/lite/text-to-image",
		SeedreamEdit:      "bytedance/seedream/v5/lite/edit",
		NanoBanana2:       "fal-ai/nano-banana-2",
		NanoBanana2Edit:   "fal-ai/nano-banana-2/edit",
		NanoBananaPro:     "fal-ai/nano-banana-pro",
		NanoBananaProEdit: "fal-ai/nano-banana-pro/edit",
		RecraftText:       "recraft/v4.1/text-to-image",
		SoulStill:         "higgsfield-ai/soul/v2/standard",
	}
}

// endpointRe is a provider path: lower-case letters, digits, dots, dashes and slashes, no scheme.
var endpointRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$`)

// ImageEndpointsFromEnv reads MF_MEDIA_ENDPOINT_* over the defaults. A bad value keeps its default
// and is listed in the error, as AudioConfigFromEnv does.
func ImageEndpointsFromEnv(get func(string) string) (ImageEndpoints, error) {
	e := DefaultImageEndpoints()
	var bad []string
	set := func(key string, dst *string) {
		v := strings.TrimSpace(get(key))
		if v == "" {
			return
		}
		if !endpointRe.MatchString(v) {
			bad = append(bad, key+" must be a provider endpoint path such as bytedance/seedream/v5/lite/text-to-image")
			return
		}
		*dst = v
	}
	set("MF_MEDIA_ENDPOINT_SEEDREAM_TEXT", &e.SeedreamText)
	set("MF_MEDIA_ENDPOINT_SEEDREAM_EDIT", &e.SeedreamEdit)
	set("MF_MEDIA_ENDPOINT_NANO_BANANA_2", &e.NanoBanana2)
	set("MF_MEDIA_ENDPOINT_NANO_BANANA_2_EDIT", &e.NanoBanana2Edit)
	set("MF_MEDIA_ENDPOINT_NANO_BANANA_PRO", &e.NanoBananaPro)
	set("MF_MEDIA_ENDPOINT_NANO_BANANA_PRO_EDIT", &e.NanoBananaProEdit)
	set("MF_MEDIA_ENDPOINT_RECRAFT_TEXT", &e.RecraftText)
	set("MF_MEDIA_ENDPOINT_SOUL_STILL", &e.SoulStill)
	if len(bad) > 0 {
		return e, fmt.Errorf("media endpoints: %s", strings.Join(bad, "; "))
	}
	return e, nil
}

// orDefault fills empty fields from the defaults, so a partly filled config never sends an empty id.
func (e ImageEndpoints) orDefault() ImageEndpoints {
	d := DefaultImageEndpoints()
	for _, f := range []struct {
		dst *string
		def string
	}{
		{&e.SeedreamText, d.SeedreamText}, {&e.SeedreamEdit, d.SeedreamEdit},
		{&e.NanoBanana2, d.NanoBanana2}, {&e.NanoBanana2Edit, d.NanoBanana2Edit},
		{&e.NanoBananaPro, d.NanoBananaPro}, {&e.NanoBananaProEdit, d.NanoBananaProEdit},
		{&e.RecraftText, d.RecraftText}, {&e.SoulStill, d.SoulStill},
	} {
		if strings.TrimSpace(*f.dst) == "" {
			*f.dst = f.def
		}
	}
	return e
}

// WithEndpoints sets the poster routes' endpoint ids.
func (p *Posters) WithEndpoints(e ImageEndpoints) *Posters {
	p.endpoints = e.orDefault()
	return p
}

// WithEndpoints sets the storyboard still routes' endpoint ids.
func (s *Storyboards) WithEndpoints(e ImageEndpoints) *Storyboards {
	s.endpoints = e.orDefault()
	return s
}
