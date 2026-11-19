package workbench

import (
	"errors"
	"net/http"

	"github.com/sylvinus/agent-vm/internal/transfermeta"
)

func transferMetadataError(w http.ResponseWriter, err error) {
	if errors.Is(err, transfermeta.ErrUnsupported) {
		http.Error(w, transfermeta.ErrUnsupported.Error(), http.StatusUnprocessableEntity)
	} else if errors.Is(err, transfermeta.ErrMalformed) {
		http.Error(w, transfermeta.ErrMalformed.Error(), http.StatusUnprocessableEntity)
	} else {
		http.Error(w, "文件元数据清理失败，请重试", http.StatusInternalServerError)
	}
}

func (s *Server) transferOptions() transfermeta.Options {
	s.mu.Lock()
	defer s.mu.Unlock()
	return transfermeta.Options{AllowUnclean: s.preferences.TransferAllowUnclean}
}
