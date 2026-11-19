package workbench

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sylvinus/agent-vm/internal/transfermeta"
)

func locationPNG(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	payload := []byte("Location\x00GPS=31.23;language=zh-CN;timezone=Asia/Shanghai")
	chunk := make([]byte, len(payload)+12)
	binary.BigEndian.PutUint32(chunk, uint32(len(payload)))
	copy(chunk[4:], "tEXt")
	copy(chunk[8:], payload)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	data := append([]byte{}, encoded.Bytes()[:encoded.Len()-12]...)
	data = append(data, chunk...)
	return append(data, encoded.Bytes()[encoded.Len()-12:]...)
}

func TestClipboardMetadata(t *testing.T) {
	root := t.TempDir()
	data := locationPNG(t)
	file := filepath.Join(root, "photo.png")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := writeClipboardArchive(&archive, []string{root}); err != nil {
		t.Fatal(err)
	}
	r := tar.NewReader(&archive)
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		clean, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(clean, []byte("Shanghai")) || bytes.Contains(clean, []byte("GPS")) || h.Size != int64(len(clean)) {
			t.Fatal("native archive preserved metadata")
		}
		if _, err := png.Decode(bytes.NewReader(clean)); err != nil {
			t.Fatal(err)
		}
	}
	if raw, err := os.ReadFile(file); err != nil || !bytes.Equal(raw, data) {
		t.Fatal("clipboard changed host source")
	}
	if err := os.WriteFile(filepath.Join(root, "blocked.pdf"), []byte("%PDF-1.7 GPS=31.23"), 0600); err != nil {
		t.Fatal(err)
	}
	archive.Reset()
	if err := writeClipboardArchive(&archive, []string{root}); err == nil {
		t.Fatal("native clipboard passed unsupported document")
	}
	archive.Reset()
	if err := writeClipboardArchive(&archive, []string{root}, transfermeta.Options{AllowUnclean: true}); err != nil {
		t.Fatal(err)
	}
	r = tar.NewReader(&archive)
	sawOriginal, sawClean := false, false
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		switch filepath.Base(h.Name) {
		case "blocked.pdf":
			sawOriginal = string(b) == "%PDF-1.7 GPS=31.23"
		case "photo.png":
			sawClean = !bytes.Contains(b, []byte("GPS"))
		}
	}
	if !sawOriginal || !sawClean {
		t.Fatal("clipboard consent bypassed cleaning or changed original")
	}
}

func TestTransferConsentSettings(t *testing.T) {
	s := testServer(t)
	if s.transferOptions().AllowUnclean {
		t.Fatal("raw transfer enabled by default")
	}
	c, err := loadConfig(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	settings := settingsView(c, s.preferences)
	settings.TransferAllowUnclean = true
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	w := request(s, "POST", "/api/settings", s.origin, string(body), true)
	if w.Code != http.StatusOK {
		t.Fatalf("settings request: %d", w.Code)
	}
	if job := awaitJobIdle(t, &s.jobs); job.Error != "" {
		t.Fatal(job.Error)
	}
	if !s.transferOptions().AllowUnclean {
		t.Fatal("explicit consent not applied")
	}
	var saved Preferences
	data, err := os.ReadFile(filepath.Join(s.profile, "workbench.json"))
	if err != nil || json.Unmarshal(data, &saved) != nil || !saved.TransferAllowUnclean {
		t.Fatal("consent not persisted")
	}
	// Old clients that omit the new option must not silently reset consent.
	var legacy map[string]any
	if err := json.Unmarshal(body, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "transfer_allow_unclean")
	legacyBody, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/settings", bytes.NewReader(legacyBody))
	r.Header.Set("Content-Type", "application/json")
	preview := httptest.NewRecorder()
	proposed, ok := s.proposed(preview, r)
	if !ok || !proposed.Preferences.TransferAllowUnclean {
		t.Fatal("omitted setting lost explicit consent")
	}
}

func TestClipboardRejectsRawArchive(t *testing.T) {
	s := testServer(t)
	s.ready = true
	if err := os.MkdirAll(s.instanceDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.instanceDir(), "cage-running.json"), []byte(fmt.Sprintf(`{"pid":%d}`, os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	data := []byte("%PDF-1.7 GPS=31.23;Asia/Shanghai")
	if err := w.WriteHeader(&tar.Header{Name: "renamed.bin", Mode: 0600, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(data)
	_ = w.Close()
	r := httptest.NewRequest("POST", "/api/clipboard/files", &archive)
	response := httptest.NewRecorder()
	s.clipboardArchive(response, r)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("raw archive reached guest import: %d", response.Code)
	}
}
