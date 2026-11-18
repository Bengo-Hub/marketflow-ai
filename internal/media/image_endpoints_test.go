package media

import "testing"

// Seedream v5 Lite has no "fal-ai/" prefix on fal (checked 2026-11-18); the prefixed id did not exist.
func TestImageEndpointDefaultsAndEnv(t *testing.T) {
	d := DefaultImageEndpoints()
	if d.SeedreamText != "bytedance/seedream/v5/lite/text-to-image" || d.SeedreamEdit != "bytedance/seedream/v5/lite/edit" {
		t.Fatalf("seedream defaults: %+v", d)
	}
	env := map[string]string{
		"MF_MEDIA_ENDPOINT_SEEDREAM_TEXT":   "bytedance/seedream/v6/text-to-image",
		"MF_MEDIA_ENDPOINT_NANO_BANANA_PRO": "https://evil.example/x",
	}
	e, err := ImageEndpointsFromEnv(func(k string) string { return env[k] })
	if err == nil {
		t.Fatal("a URL is not an endpoint path and must be reported")
	}
	if e.SeedreamText != "bytedance/seedream/v6/text-to-image" {
		t.Fatalf("a valid override wins: %q", e.SeedreamText)
	}
	if e.NanoBananaPro != d.NanoBananaPro {
		t.Fatalf("a bad override keeps the default: %q", e.NanoBananaPro)
	}
	if got := (ImageEndpoints{}).orDefault(); got != d {
		t.Fatalf("an empty config falls back to the defaults: %+v", got)
	}
}
