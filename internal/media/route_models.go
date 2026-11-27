package media

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Media route models (owner, 2026-11-28): the provider model id behind each media tier is a
// platform setting, edited on Platform > Providers (the fal.ai and ElevenLabs cards) and stored in
// the platform ProviderSetting media/routes, one key per tier. A key that is not set keeps the
// route's own default (the MF_MEDIA_ENDPOINT_* value or the code default), so the defaults below
// are today's routes. A configured id the provider's model list does not hold, or one in the wrong
// category, is ignored with an error in the log and the default runs: a typo never stalls a render.
//
// Voice is ElevenLabs direct first, always (owner, 2026-11-28): library and cloned voices and exact
// word timings need it. fal's ElevenLabs routes are the fallback only, matched by tier (eleven-v4
// for premium, eleven-v4-turbo for standard), when ElevenLabs is down, out of credit, has no valid
// key or its breaker is open.
const (
	RouteKeyPosterPremium    = "media.route.poster_premium"
	RouteKeyPosterStandard   = "media.route.poster_standard"
	RouteKeyVideoPremium     = "media.route.video_premium"
	RouteKeyVideoStandard    = "media.route.video_standard"
	RouteKeyPresenter        = "media.route.presenter"
	RouteKeyPresenterFabric  = "media.route.presenter_fabric"
	RouteKeyVoicePremium     = "media.route.voice_premium"
	RouteKeyVoiceStandard    = "media.route.voice_standard"
	RouteKeyVoiceFalPremium  = "media.route.voice_fal_premium"
	RouteKeyVoiceFalStandard = "media.route.voice_fal_standard"
	// RouteKeyTimingsViaScribe ("true" or "false") times fal voice-overs that come back without
	// timings with fal's Scribe v2 speech to text (USD 0.008 a minute) instead of an estimate.
	RouteKeyTimingsViaScribe = "media.route.voice_timings_via_scribe"
)

// Default fal voice endpoints: ElevenLabs v4 on fal (USD 0.08 and 0.04 per 1,000 characters,
// fal pricing API 2026-11-28). They replaced fal-ai/elevenlabs/tts/eleven-v3 (USD 0.10).
const (
	defaultFalVoicePremium  = "elevenlabs/tts/eleven-v4"
	defaultFalVoiceStandard = "elevenlabs/tts/eleven-v4-turbo"
	// scribeEndpoint is fal's ElevenLabs speech to text, for word timings.
	scribeEndpoint = "fal-ai/elevenlabs/speech-to-text/scribe-v2"
	// scribePricePerMinute is its price in USD (fal pricing API 2026-11-28).
	scribePricePerMinute = 0.008
)

// RouteModels are the configured model ids, already checked; "" means the route's default.
type RouteModels struct {
	PosterPremium    string // fal text to image (nano-banana-pro); its /edit sibling serves photos
	PosterStandard   string // fal text to image (seedream); its /edit sibling serves photos
	VideoPremium     string // fal image to video (route fal/kling-v3-pro)
	VideoStandard    string // fal image to video (route fal/hailuo-2.3-fast)
	Presenter        string // fal presenter (route fal/creatify-boreal)
	PresenterFabric  string // fal presenter (route fal/veed-fabric)
	VoicePremium     string // ElevenLabs direct model id (eleven_v4)
	VoiceStandard    string // ElevenLabs direct model id (eleven_v4_turbo)
	VoiceFalPremium  string // fal text to speech (elevenlabs/tts/eleven-v4)
	VoiceFalStandard string // fal text to speech (elevenlabs/tts/eleven-v4-turbo)
	TimingsViaScribe bool
}

// slotKind says how a slot's value is checked.
type slotKind int

const (
	slotFal    slotKind = iota // a fal endpoint id, checked against fal's model list
	slotEleven                 // an ElevenLabs model id
)

type routeSlot struct {
	key        string
	kind       slotKind
	categories []string // fal categories the id may have ("" in the list: not checked)
	field      func(*RouteModels) *string
}

// routeSlots are every model slot. Clip routes animate a still, so only image to video fits them.
var routeSlots = []routeSlot{
	{RouteKeyPosterPremium, slotFal, []string{"text-to-image", "image-to-image"}, func(m *RouteModels) *string { return &m.PosterPremium }},
	{RouteKeyPosterStandard, slotFal, []string{"text-to-image", "image-to-image"}, func(m *RouteModels) *string { return &m.PosterStandard }},
	{RouteKeyVideoPremium, slotFal, []string{"image-to-video"}, func(m *RouteModels) *string { return &m.VideoPremium }},
	{RouteKeyVideoStandard, slotFal, []string{"image-to-video"}, func(m *RouteModels) *string { return &m.VideoStandard }},
	{RouteKeyPresenter, slotFal, []string{"image-to-video", "audio-to-video"}, func(m *RouteModels) *string { return &m.Presenter }},
	{RouteKeyPresenterFabric, slotFal, []string{"image-to-video", "audio-to-video"}, func(m *RouteModels) *string { return &m.PresenterFabric }},
	{RouteKeyVoicePremium, slotEleven, nil, func(m *RouteModels) *string { return &m.VoicePremium }},
	{RouteKeyVoiceStandard, slotEleven, nil, func(m *RouteModels) *string { return &m.VoiceStandard }},
	{RouteKeyVoiceFalPremium, slotFal, []string{"text-to-speech"}, func(m *RouteModels) *string { return &m.VoiceFalPremium }},
	{RouteKeyVoiceFalStandard, slotFal, []string{"text-to-speech"}, func(m *RouteModels) *string { return &m.VoiceFalStandard }},
}

// elevenModelRe is an ElevenLabs model id (eleven_v4, eleven_v4_turbo, eleven_multilingual_v2).
var elevenModelRe = regexp.MustCompile(`^eleven_[a-z0-9_]+$`)

// FalLookup answers whether fal lists an endpoint. known is false until a list has been read, and
// then nothing is refused for being missing (the format check still applies).
type FalLookup interface {
	Lookup(id string) (category string, listed, known bool)
}

// RouteModelsFromSettings reads the media/routes settings. A value that fails its check is left
// out (the default runs) and logged as an error, once per value while it stays wrong.
func RouteModelsFromSettings(settings map[string]string, fal FalLookup, logger *zap.Logger) RouteModels {
	var m RouteModels
	for _, s := range routeSlots {
		v := strings.TrimSpace(settings[s.key])
		if v == "" {
			continue
		}
		if reason := checkSlot(s, v, fal); reason != "" {
			warnBadRoute(logger, s.key, v, reason)
			continue
		}
		*s.field(&m) = v
	}
	m.TimingsViaScribe = parseFlag(settings[RouteKeyTimingsViaScribe])
	return m
}

// checkSlot returns why v cannot fill the slot, or "".
func checkSlot(s routeSlot, v string, fal FalLookup) string {
	switch s.kind {
	case slotEleven:
		if !elevenModelRe.MatchString(v) {
			return "not an ElevenLabs model id (eleven_...)"
		}
		return ""
	}
	if !endpointRe.MatchString(v) {
		return "not a fal endpoint id"
	}
	if fal == nil {
		return ""
	}
	cat, listed, known := fal.Lookup(v)
	if !known {
		return ""
	}
	if !listed {
		return "fal's model list does not have it"
	}
	if cat != "" && len(s.categories) > 0 && !containsString(s.categories, cat) {
		return "fal lists it as " + cat + ", not " + strings.Join(s.categories, " or ")
	}
	return ""
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func parseFlag(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "on", "yes", "1":
		return true
	}
	return false
}

var (
	badRouteMu   sync.Mutex
	badRouteSeen = map[string]time.Time{}
)

// warnBadRoute logs a refused value loudly, at most once an hour per key and value, so a wrong
// setting is seen without a log line on every render.
func warnBadRoute(logger *zap.Logger, key, value, reason string) {
	if logger == nil {
		return
	}
	id := key + "=" + value
	badRouteMu.Lock()
	last, seen := badRouteSeen[id]
	if seen && time.Since(last) < time.Hour {
		badRouteMu.Unlock()
		return
	}
	badRouteSeen[id] = time.Now()
	badRouteMu.Unlock()
	logger.Error("media route setting refused; the route's default model runs instead",
		zap.String("key", key), zap.String("model", value), zap.String("reason", reason))
}

// RouteModelSource returns the configured route models (app wiring caches it).
type RouteModelSource func(ctx context.Context) RouteModels

// routeModelSource is the source every pipeline reads, and routeFal the fal list a derived
// endpoint (a poster model's /edit sibling) is checked against; app wiring sets both once at start.
var (
	routeModelSource RouteModelSource
	routeFal         FalLookup
)

// SetRouteModelSource sets where the route models come from (nil: every route on its default) and
// the fal model list derived endpoints are checked against (nil: not checked).
func SetRouteModelSource(src RouteModelSource, fal FalLookup) {
	routeModelSource, routeFal = src, fal
}

// currentRouteModels is the route models in force now.
func currentRouteModels(ctx context.Context) RouteModels {
	if routeModelSource == nil {
		return RouteModels{}
	}
	return routeModelSource(ctx)
}

// routeEndpoint is the configured fal endpoint for a route key, or fallback when none is set.
func routeEndpoint(ctx context.Context, routeKey, fallback string) string {
	m := currentRouteModels(ctx)
	var v string
	switch routeKey {
	case "fal/kling-v3-pro":
		v = m.VideoPremium
	case "fal/hailuo-2.3-fast":
		v = m.VideoStandard
	case "fal/creatify-boreal":
		v = m.Presenter
	case "fal/veed-fabric":
		v = m.PresenterFabric
	}
	if v == "" {
		return fallback
	}
	return v
}

// imagePair is the text to image and edit endpoints for a configured poster model. The setting
// names one of the pair; the other is its sibling (".../text-to-image" and ".../edit", or "x"
// and "x/edit"). With nothing configured the route's own pair stays.
func imagePair(configured, text, edit string) (string, string) {
	c := strings.TrimSpace(configured)
	switch {
	case c == "":
		return text, edit
	case strings.HasSuffix(c, "/edit"):
		base := strings.TrimSuffix(c, "/edit")
		if strings.HasSuffix(text, "/text-to-image") {
			return base + "/text-to-image", c
		}
		return base, c
	case strings.HasSuffix(c, "/text-to-image"):
		return c, strings.TrimSuffix(c, "/text-to-image") + "/edit"
	default:
		return c, c + "/edit"
	}
}

// posterEndpoints is imagePair with each derived endpoint checked against fal's list: a sibling
// fal does not list falls back to the route's own endpoint for that variant.
func posterEndpoints(configured, text, edit string) (string, string) {
	t, e := imagePair(configured, text, edit)
	if !falListed(t) {
		t = text
	}
	if !falListed(e) {
		e = edit
	}
	return t, e
}

// falListed is false only when fal's list has been read and lacks id.
func falListed(id string) bool {
	if routeFal == nil {
		return true
	}
	_, listed, known := routeFal.Lookup(id)
	return listed || !known
}

// falVoiceEndpoint is the fal text to speech endpoint for a voice route.
func falVoiceEndpoint(ctx context.Context, routeKey string) string {
	m := currentRouteModels(ctx)
	if routeKey == falVoiceRoute {
		return firstNonEmptyModel(m.VoiceFalPremium, defaultFalVoicePremium)
	}
	return firstNonEmptyModel(m.VoiceFalStandard, defaultFalVoiceStandard)
}
