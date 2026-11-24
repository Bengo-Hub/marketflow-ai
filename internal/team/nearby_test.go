package team

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/agent"
)

type fakeSearch struct {
	calls int
	reply string
	err   error
}

func (*fakeSearch) Name() string           { return "web_search" }
func (*fakeSearch) Description() string    { return "" }
func (*fakeSearch) Schema() map[string]any { return nil }
func (f *fakeSearch) Invoke(context.Context, uuid.UUID, string) (string, error) {
	f.calls++
	return f.reply, f.err
}

type memNearby struct {
	vals map[string]string
	ttl  map[string]time.Duration
}

func (m *memNearby) Get(_ context.Context, k string) (string, bool) { v, ok := m.vals[k]; return v, ok }
func (m *memNearby) Set(_ context.Context, k, v string, ttl time.Duration) {
	m.vals[k], m.ttl[k] = v, ttl
}

const kisumuAddress = "Pioneer House, Suite 203A, Oginga-Odinga Road, Kisumu CBD, Kenya"

// The lookup runs once per tenant and address and is then read from the shared cache.
func TestNearbyNotesCachedPerTenant(t *testing.T) {
	search := &fakeSearch{reply: `{"answer":"Mega Plaza towers rise over Oginga Odinga Street.","results":[{"title":"x","content":"Matatus   and boda bodas crowd the CBD."}]}`}
	cache := &memNearby{vals: map[string]string{}, ttl: map[string]time.Duration{}}
	rt := New(Config{Tools: []agent.Tool{search}, Nearby: cache})
	tenant := uuid.New()
	p := PlaceFacts{Address: kisumuAddress, LookupNearby: true}
	got := rt.nearbyNotes(context.Background(), tenant, p)
	if !strings.Contains(got, "Mega Plaza") || !strings.Contains(got, "Matatus and boda bodas") {
		t.Fatalf("notes %q", got)
	}
	if again := rt.nearbyNotes(context.Background(), tenant, p); again != got || search.calls != 1 {
		t.Fatalf("second read searched again: %d calls", search.calls)
	}
	for _, ttl := range cache.ttl {
		if ttl != nearbyTTL {
			t.Fatalf("ttl %v", ttl)
		}
	}
	// Turned off, no address, no cache or no search tool: no lookup.
	for _, c := range []struct {
		rt *Runtime
		p  PlaceFacts
	}{
		{rt, PlaceFacts{Address: kisumuAddress}},
		{rt, PlaceFacts{LookupNearby: true}},
		{New(Config{Tools: []agent.Tool{search}}), p},
		{New(Config{Nearby: cache}), p},
	} {
		if c.rt.nearbyNotes(context.Background(), uuid.New(), c.p) != "" {
			t.Fatalf("lookup ran for %+v", c.p)
		}
	}
	if search.calls != 1 {
		t.Fatalf("calls %d", search.calls)
	}
	// A failed search is not cached; an empty answer is, for a day.
	failing := &fakeSearch{err: errors.New("tavily daily cap reached")}
	rt = New(Config{Tools: []agent.Tool{failing}, Nearby: cache})
	if rt.nearbyNotes(context.Background(), uuid.New(), p) != "" || len(cache.vals) != 1 {
		t.Fatalf("failure cached: %v", cache.vals)
	}
	empty := &fakeSearch{reply: `{"answer":"","results":[]}`}
	rt = New(Config{Tools: []agent.Tool{empty}, Nearby: cache})
	rt.nearbyNotes(context.Background(), uuid.New(), p)
	if len(cache.vals) != 2 {
		t.Fatalf("empty answer not cached: %v", cache.vals)
	}
}

// The address the account gives is kept exactly; the area is never the country.
func TestAnchorAddressKeepsTheSSOAddress(t *testing.T) {
	b := &SettingBrief{HomeBase: SettingNote{Place: "Pioneer House, Oginga Street"},
		Settings: []SettingNote{
			{Place: "the street outside", Neighbourhood: "Oginga Odinga Road, Kisumu CBD"},
			{Place: "a learning room", Neighbourhood: "Moi Avenue, Kisumu"},
		}}
	anchorAddress(b, PlaceFacts{Address: kisumuAddress, Country: "Kenya"})
	if b.HomeBase.Place != kisumuAddress || b.HomeBase.Neighbourhood != "" {
		t.Fatalf("home base %+v", b.HomeBase)
	}
	if b.Settings[0].Neighbourhood != "Oginga Odinga Road, Kisumu CBD" {
		t.Fatalf("the real road written without its hyphen was moved: %+v", b.Settings[0])
	}
	if b.Settings[1].Neighbourhood != "Kisumu CBD" {
		t.Fatalf("an invented street should move to the area, got %+v", b.Settings[1])
	}
	if !mentionsPlace("a cafe on Oginga Odinga Road", b) {
		t.Fatal("the writer's line naming the road should count as the place")
	}
}

func TestSettingTaskCarriesTheAreaAndPlaceRules(t *testing.T) {
	task := settingTask(VideoScriptRequest{Offering: "Digitika Kids & Teens", Place: PlaceFacts{Address: kisumuAddress, BusinessType: "education",
		Area:   &PlaceArea{Name: "kisumu cbd", Landmarks: []string{"the twin towers of Mega Plaza"}, StreetLife: "matatus and boda bodas"},
		Nearby: "The clock tower stands at Kampala Road."}})
	for _, want := range []string{"Business type: education", "landmark: the twin towers of Mega Plaza", "street life: matatus",
		"Web notes", "Never put the business inside a landmark", "robotics kits", "Copy the address exactly"} {
		if !strings.Contains(task, want) {
			t.Fatalf("task lacks %q:\n%s", want, task)
		}
	}
}

// Scenes of one kind take turns over the settings that fit them, so the places vary.
func TestApplySettingsVariesThePlaces(t *testing.T) {
	b := &SettingBrief{HomeBase: SettingNote{Place: kisumuAddress},
		Settings: []SettingNote{
			{Key: "street", Place: "Oginga Odinga Road outside", Fits: "b_roll"},
			{Key: "lab", Place: "a bright learning room", Fits: "b_roll, presenter"},
		}}
	if err := b.clean(); err != nil {
		t.Fatal(err)
	}
	v := &VideoScript{Scenes: []Scene{{ID: "s1", Shot: "b_roll"}, {ID: "s2", Shot: "b_roll"}, {ID: "s3", Shot: "b_roll"}}}
	applySettings(v, b)
	if v.Scenes[0].Setting == v.Scenes[1].Setting || v.Scenes[0].Setting != v.Scenes[2].Setting {
		t.Fatalf("settings %q %q %q", v.Scenes[0].Setting, v.Scenes[1].Setting, v.Scenes[2].Setting)
	}
}

// Without place facts a re-set changes nothing.
func TestResetSettingsWithoutPlaceKeepsTheScenes(t *testing.T) {
	scenes := []Scene{{ID: "s1", Shot: "b_roll", Setting: "a learning room", Voiceover: "Hello"}}
	got, brief, changed := New(Config{}).ResetSettings(context.Background(), VideoScriptRequest{}, scenes)
	if brief != nil || changed != 0 || got[0].Setting != "a learning room" || got[0].Voiceover != "Hello" {
		t.Fatalf("got %+v %v %d", got, brief, changed)
	}
}

// Owner, 2026-11-24: settings leaked screens with content and calls to action. Every field drops them.
func TestSettingsDropScreensCTAsAndNumbers(t *testing.T) {
	b := &SettingBrief{Settings: []SettingNote{
		{Place: "POS workstation, Kisumu CBD shop interior", Vibe: "focused close-up lighting on screen, calm", People: "hands of the owner",
			Props: "tablet, split-payment screen, KRA eTIMS invoice preview, Book a demo call-to-action"},
		{Place: "Digitika learning room", Props: "Scratch project screen, coding wall, laptops, robotics kits", People: "children, call-to-action text with WhatsApp number"},
	}}
	if err := b.clean(); err != nil {
		t.Fatal(err)
	}
	line := strings.ToLower(b.Settings[0].Line() + " " + b.Settings[1].Line())
	for _, bad := range []string{"split-payment", "invoice", "preview", "call-to-action", "whatsapp", "number", "screen"} {
		if strings.Contains(line, bad) {
			t.Fatalf("%q leaked: %s", bad, line)
		}
	}
	for _, keep := range []string{"tablet", "laptops", "robotics kits", "coding wall", "pos workstation"} {
		if !strings.Contains(line, keep) {
			t.Fatalf("%q lost: %s", keep, line)
		}
	}
}
