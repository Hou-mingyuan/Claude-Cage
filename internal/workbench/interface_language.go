package workbench

import (
	"net/http"
	"path/filepath"

	"github.com/sylvinus/agent-vm/internal/uilocale"
)

func (s *Server) interfaceLanguage(w http.ResponseWriter, r *http.Request) {
	profile := s.profile
	if s.appUpdateConfigPath != "" {
		profile = filepath.Dir(s.appUpdateConfigPath)
	}
	path := filepath.Join(profile, uilocale.PreferenceFile)
	var pref uilocale.Preference
	if r.Method == http.MethodPost {
		if !readJSON(w, r, &pref, 1024) {
			return
		}
		if !uilocale.Valid(pref.Language) {
			http.Error(w, "请选择支持的界面语言。", http.StatusBadRequest)
			return
		}
		if err := writePrivate(path, pref); err != nil {
			http.Error(w, "无法保存界面语言，请重试。", http.StatusInternalServerError)
			return
		}
	} else {
		pref = uilocale.Load(profile)
	}
	system := uilocale.Host()
	jsonReply(w, struct {
		uilocale.Preference
		Active string `json:"active"`
		System string `json:"system"`
	}{pref, pref.Active(system), system})
}
