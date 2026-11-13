package team

// FitTemplate is fitTemplate for callers outside the team: it makes a script renderable as a
// Studio video ad for the given photos and footage (a presenter ad keeps its presenter scenes).
// marketflow-ai's make-media step uses it on a Marketing Manager turn's script.
func FitTemplate(v *VideoScript, photos, footage int, presenter bool) {
	fitTemplate(v, photos, footage, presenter)
}
