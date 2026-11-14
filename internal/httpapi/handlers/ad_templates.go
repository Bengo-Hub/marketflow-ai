package handlers

import (
	"net/http"
)

// adTemplateView is one ad template as the Studio's picker shows it.
type adTemplateView struct {
	Key       string   `json:"key"`
	Name      string   `json:"name"`
	WhenToUse string   `json:"when_to_use"`
	Kinds     []string `json:"kinds"`
	TwoVoice  bool     `json:"two_voice"`
	Requires  []string `json:"requires,omitempty"`
}

// AdTemplates handles GET /api/v1/internal/creative/ad-templates (service key): the ad templates
// (library ad_templates.yaml) and each video kind's default, for the Studio's template picker.
// The list is data, so a new template shows in the picker without a UI change.
func (h *OSHandler) AdTemplates(w http.ResponseWriter, r *http.Request) {
	if h.lib == nil {
		writeError(w, http.StatusServiceUnavailable, "the template library is not loaded")
		return
	}
	list := h.lib.AdTemplates()
	out := make([]adTemplateView, 0, len(list))
	for _, t := range list {
		out = append(out, adTemplateView{Key: t.Key, Name: t.Name, WhenToUse: t.WhenToUse, Kinds: t.Kinds,
			TwoVoice: t.TwoVoice, Requires: t.Requires})
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out, "defaults": h.lib.AdTemplateDefaults()})
}
