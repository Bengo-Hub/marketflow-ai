# Media route models

Last updated 2026-11-28.

I keep the model behind each media tier in platform settings, not in code. The owner sets them on Platform > Providers, on the fal.ai and ElevenLabs cards. They live in the platform ProviderSetting `media/routes`, one key per tier, and marketflow-api merges each save into that row (`PATCH /api/v1/admin/providers/media-routes`).

Code: `internal/media/route_models.go` (keys, checks, how each route reads its model), `internal/media/fal_catalog.go` (fal's model list), `internal/app/media.go` (`mediaRouteModelSource`).

## Keys and defaults

A key that is not set runs the default, so the defaults are the routes as they were before this change. The one exception is the fal voice fallback, which moved from `fal-ai/elevenlabs/tts/eleven-v3` (USD 0.10 per 1,000 characters) to ElevenLabs v4 on fal.

| Key | Route it sets | Default |
|---|---|---|
| `media.route.poster_premium` | `fal/nano-banana-pro` | `fal-ai/nano-banana-pro` (its `/edit` sibling with photos) |
| `media.route.poster_standard` | `fal/seedream-v5-lite` | `bytedance/seedream/v5/lite/text-to-image` (its `/edit` sibling with photos) |
| `media.route.video_premium` | `fal/kling-v3-pro` | `fal-ai/kling-video/v3/pro/image-to-video` |
| `media.route.video_standard` | `fal/hailuo-2.3-fast` | `fal-ai/minimax/hailuo-2.3-fast/standard/image-to-video` |
| `media.route.presenter` | `fal/creatify-boreal` | `creatify/boreal` |
| `media.route.presenter_fabric` | `fal/veed-fabric` | `veed/fabric-1.0` |
| `media.route.voice_premium` | `elevenlabs/eleven-v4` (direct) | `eleven_v4` |
| `media.route.voice_standard` | `elevenlabs/eleven-v4-turbo` (direct) | `eleven_v4_turbo` |
| `media.route.voice_fal_premium` | `fal/elevenlabs-v4` | `elevenlabs/tts/eleven-v4` (USD 0.08 per 1,000 characters) |
| `media.route.voice_fal_standard` | `fal/elevenlabs-v4-turbo` | `elevenlabs/tts/eleven-v4-turbo` (USD 0.04 per 1,000 characters) |
| `media.route.voice_timings_via_scribe` | timings of a fal voice-over | `false` |

The MF_MEDIA_ENDPOINT_* variables still set the poster endpoints; a platform setting wins over them. The ElevenLabs card's older `voice_model` and `voice_premium_model` stay as the fallback for the two direct voice keys.

A configured model changes the endpoint only. Each route keeps its own request shape, price and timeout, so a new model must take the same input as the one it replaces. That is why the console asks for a short test render before a switch.

## Checks, and why nothing stalls

- The settings are read through the resolver with a one minute cache in memory (and the resolver's five minute Redis cache), so a save takes effect within about five minutes. A failed read keeps the last good settings.
- Every read checks each value: a fal id must look like an endpoint id, and once fal's model list is loaded it must be listed in a category that fits the tier (posters text-to-image or image-to-image, clips image-to-video, presenters image-to-video or audio-to-video, fal voices text-to-speech). An ElevenLabs id must look like `eleven_...`.
- A value that fails runs the route's default, and I log it as an error once an hour per key and value.
- fal's list (about 1,400 endpoints) is read in the background with the platform's fal key and kept for six hours. A lookup never waits for it. Until the first read works, a well formed id is trusted. A failed read backs off from 5 minutes to an hour with jitter.
- A poster setting names one of a text-to-image and edit pair, and the other is derived. When fal's list lacks the derived sibling, that variant keeps the route's own endpoint.

`creatify/boreal` is not in fal's public model list (checked 2026-11-28), so the picker shows it as "not in the list". It still works as the default.

## Voice: ElevenLabs direct first

ElevenLabs direct is always tried first. Only it reads library and cloned voices and returns exact word timings. fal's ElevenLabs routes are fallbacks, matched by tier: `fal/elevenlabs-v4` for premium, and `fal/elevenlabs-v4-turbo` for standard and as the last premium fallback. They run when ElevenLabs is down, out of credit, on the free plan, without a valid key or with its breaker open. fal reads premade voices only, so a library voice is replaced by the nearest premade one and the result records `substituted_for`.

On fal I ask for timestamps. When fal returns usable timings the captions use them. When it does not and `media.route.voice_timings_via_scribe` is on, `fal-ai/elevenlabs/speech-to-text/scribe-v2` (USD 0.008 a minute) times the audio within 30 seconds and its cost is added to the voice-over. Otherwise, or when Scribe fails, the timings are estimated and the assembler's re-alignment places them on the speech. The result says where the timings came from (`timings`: provider, scribe or estimated).

The owner first asked for a setting to prefer fal for premade voices. On 28 November 2026 the owner decided against it: ElevenLabs direct is always preferred, so that setting does not exist.

## Upgrades

When the owner presses Sync models on the fal.ai or ElevenLabs card, marketflow-api moves a tier to a newer version of the same family and variant (for example `eleven_v4` to `eleven_v5`, or `elevenlabs/tts/eleven-v4-turbo` to `eleven-v5-turbo`), keeps the replaced id for revert and tells the platform admins on Slack and by email. A different family, another variant or a preview build stays a recommendation. The rules are in marketflow-api `internal/platform/modelcatalog/autopick.go`.
