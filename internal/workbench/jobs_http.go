package workbench

import (
	"errors"
	"net/http"
	"strings"
)

func requestInitiator(r *http.Request) string {
	if r == nil {
		return jobInitiatorDesktop
	}
	switch r.Header.Get(jobInitiatorHeader) {
	case jobInitiatorAgent:
		return jobInitiatorAgent
	case jobInitiatorSetup:
		return jobInitiatorSetup
	case jobInitiatorDesktop:
		return jobInitiatorDesktop
	}
	if strings.HasPrefix(r.URL.Path, "/api/agent/") {
		return jobInitiatorAgent
	}
	return jobInitiatorDesktop
}

func (s *Server) launch(w http.ResponseWriter, kind string, run func() error) {
	s.launchFrom(w, jobInitiatorDesktop, kind, run)
}

func (s *Server) launchRequest(w http.ResponseWriter, r *http.Request, kind string, run func() error) {
	s.launchFrom(w, requestInitiator(r), kind, run)
}

func (s *Server) launchFrom(w http.ResponseWriter, initiator, kind string, run func() error) {
	job, err := s.submitJobWithInitiator(initiator, kind, run)
	switch {
	case errors.Is(err, errJobsClosed):
		http.Error(w, "后台正在更新，请重新打开 Cage。", http.StatusServiceUnavailable)
	case errors.Is(err, errJobBusy):
		http.Error(w, "另一个操作仍在进行，请稍候", http.StatusConflict)
	case err != nil:
		requestError(w, err)
	default:
		jsonReply(w, job)
	}
}
