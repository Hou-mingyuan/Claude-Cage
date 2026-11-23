package workbench

import (
	"net/http"
	"strings"

	"github.com/sylvinus/agent-vm/internal/remotehost"
)

// This handler is reached through the ordinary local session/Origin boundary.
// No caller-supplied path, raw config, private key, or command is accepted.
func (s *Server) clientSSHConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "请求方法不支持", http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		Aliases  []string `json:"aliases"`
		Revision string   `json:"revision"`
	}
	if r.Method == http.MethodPost && !readJSON(w, r, &input) {
		return
	}
	preview, err := remotehost.ReadSSHConfig()
	if err != nil {
		requestError(w, err)
		return
	}
	if r.Method == http.MethodGet {
		jsonReply(w, preview)
		return
	}
	if input.Revision == "" || input.Revision != preview.Revision {
		http.Error(w, "SSH 配置已变化，请重新打开导入预览", http.StatusConflict)
		return
	}
	if len(input.Aliases) == 0 || len(input.Aliases) > 100 {
		http.Error(w, "请选择 1 至 100 个 SSH 主机", http.StatusBadRequest)
		return
	}
	byAlias := map[string]remotehost.HostProfile{}
	for _, p := range preview.Hosts {
		byAlias[strings.ToLower(p.Name)] = p
	}
	selected := []remotehost.HostProfile{}
	seen := map[string]bool{}
	for _, name := range input.Aliases {
		key := strings.ToLower(name)
		p, ok := byAlias[key]
		if !ok || seen[key] {
			http.Error(w, "SSH 主机选择无效，请重新打开导入预览", http.StatusBadRequest)
			return
		}
		seen[key] = true
		selected = append(selected, p)
	}
	added, err := s.remote.store.ImportSSHHosts(selected)
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, map[string]any{"imported": added, "skipped": len(selected) - len(added)})
}
