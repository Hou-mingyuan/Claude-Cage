package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

const verifier = "http://127.0.0.1:18156"

var proofClient = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func protectionConfig(w http.ResponseWriter, r *http.Request) {
	req, err := http.NewRequestWithContext(r.Context(), "GET", verifier+"/local/cage-config", nil)
	if err != nil {
		fail(w, 503, "安全验证暂不可用。")
		return
	}
	resp, err := proofClient.Do(req)
	if err != nil {
		fail(w, 503, "安全验证暂不可用。")
		return
	}
	defer resp.Body.Close()
	var cfg struct {
		Enabled *bool  `json:"enabled"`
		SiteKey string `json:"siteKey"`
		Region  string `json:"region"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 2048)).Decode(&cfg) != nil || cfg.Enabled == nil {
		fail(w, 503, "安全验证暂不可用。")
		return
	}
	respond(w, 200, cfg)
}

func validatePageProof(ctx context.Context, proof, action string) (bool, error) {
	if len(proof) > 2048 {
		return false, nil
	}
	content, _ := json.Marshal(map[string]string{"proof": proof, "action": action})
	req, err := http.NewRequestWithContext(ctx, "POST", verifier+"/local/cage-proof", bytes.NewReader(content))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := proofClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 403 {
		return false, nil
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 512)).Decode(&result) != nil || !result.OK {
		return false, errors.New("verification service unavailable")
	}
	return true, nil
}
