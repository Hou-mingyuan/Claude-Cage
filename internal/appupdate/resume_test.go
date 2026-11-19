package appupdate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rangeSource serves content like the release CDN and can cut or hang a body.
type rangeSource struct {
	content  string
	requests []string
	limit    func(request int) int // bytes served before the connection drops; <0 serves all
	hang     func(request int) bool
}

type cutBody struct {
	io.Reader
	hang context.Context
}

func (b *cutBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF && n == 0 {
		if b.hang != nil {
			<-b.hang.Done()
			return 0, b.hang.Err()
		}
		return 0, io.ErrUnexpectedEOF
	}
	return n, nil
}
func (b *cutBody) Close() error { return nil }

func (s *rangeSource) RoundTrip(r *http.Request) (*http.Response, error) {
	index := len(s.requests)
	header := r.Header.Get("Range")
	s.requests = append(s.requests, header)
	offset, status := 0, 200
	responseHeader := http.Header{}
	if header != "" {
		offset, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(header, "bytes="), "-"))
		status = 206
		responseHeader.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(s.content)-1, len(s.content)))
	}
	rest := s.content[offset:]
	resp := &http.Response{StatusCode: status, Header: responseHeader, ContentLength: int64(len(rest)), Request: r}
	served := -1
	if s.limit != nil {
		served = s.limit(index)
	}
	switch {
	case s.hang != nil && s.hang(index):
		resp.Body = &cutBody{Reader: strings.NewReader(rest[:served]), hang: r.Context()}
	case served >= 0 && served < len(rest):
		resp.Body = &cutBody{Reader: strings.NewReader(rest[:served])}
	default:
		resp.Body = io.NopCloser(strings.NewReader(rest))
	}
	return resp, nil
}

func resumeFixture(t *testing.T) (asset, string, string, string) {
	t.Helper()
	downloadBackoff = 0
	content := strings.Repeat("Cage archive ", 64)
	a := fixtureRelease("0.2.0", false).Assets[0]
	a.Size = int64(len(content))
	return a, content, fmt.Sprintf("%x", sha256.Sum256([]byte(content))), filepath.Join(t.TempDir(), "Cage.dmg")
}

func TestDroppedDownloadContinuesFromStoredBytes(t *testing.T) {
	a, content, digest, path := resumeFixture(t)
	source := &rangeSource{content: content, limit: func(request int) int {
		if request < 2 {
			return 100
		}
		return -1
	}}
	retries := 0
	if err := download(context.Background(), &http.Client{Transport: source}, a, digest, path, func(int64) {}, func() { retries++ }); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != content {
		t.Fatal("resumed archive differs")
	}
	if strings.Join(source.requests, ",") != ",bytes=100-,bytes=200-" || retries != 2 {
		t.Fatalf("requests=%q retries=%d", source.requests, retries)
	}
}

func TestInterruptedDownloadResumesInALaterRun(t *testing.T) {
	a, content, digest, path := resumeFixture(t)
	source := &rangeSource{content: content, limit: func(int) int { return 50 }}
	downloadAttempts = 2
	defer func() { downloadAttempts = 6 }()
	if err := download(context.Background(), &http.Client{Transport: source}, a, digest, path, func(int64) {}, func() {}); err == nil {
		t.Fatal("incomplete download passed")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("incomplete archive published")
	}
	// A partial file of another release must not be continued.
	stale := path + "." + strings.Repeat("0", 16) + ".part"
	if err := os.WriteFile(stale, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	later := &rangeSource{content: content}
	var first int64 = -1
	if err := download(context.Background(), &http.Client{Transport: later}, a, digest, path, func(n int64) {
		if first < 0 {
			first = n
		}
	}, func() {}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != content || len(later.requests) != 1 || later.requests[0] != "bytes=100-" || first != 100 {
		t.Fatalf("requests=%q first=%d", later.requests, first)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale partial file retained")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestCorruptPartialFileIsDiscardedAfterDigestMismatch(t *testing.T) {
	a, content, digest, path := resumeFixture(t)
	partial := path + "." + digest[:16] + ".part"
	if err := os.WriteFile(partial, []byte(strings.Repeat("x", 100)), 0600); err != nil {
		t.Fatal(err)
	}
	source := &rangeSource{content: content}
	if err := download(context.Background(), &http.Client{Transport: source}, a, digest, path, func(int64) {}, func() {}); err == nil {
		t.Fatal("corrupt archive passed")
	}
	for _, name := range []string{path, partial} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatal("corrupt data retained: " + name)
		}
	}
}

func TestStalledDownloadIsRetriedAndCancellationKeepsProgress(t *testing.T) {
	a, content, digest, path := resumeFixture(t)
	downloadStall = 50 * time.Millisecond
	defer func() { downloadStall = 60 * time.Second }()
	source := &rangeSource{content: content, hang: func(request int) bool { return request == 0 }, limit: func(request int) int {
		if request == 0 {
			return 100
		}
		return -1
	}}
	if err := download(context.Background(), &http.Client{Transport: source}, a, digest, path, func(int64) {}, func() {}); err != nil {
		t.Fatal(err)
	}
	if len(source.requests) != 2 || source.requests[1] != "bytes=100-" {
		t.Fatalf("requests=%q", source.requests)
	}
	os.Remove(path)
	ctx, cancel := context.WithCancel(context.Background())
	downloadStall = time.Minute
	hanging := &rangeSource{content: content, limit: func(int) int { return 100 }, hang: func(int) bool { return true }}
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if err := download(ctx, &http.Client{Transport: hanging}, a, digest, path, func(int64) {}, func() {}); err == nil || !strings.Contains(err.Error(), "已取消") {
		t.Fatal(err)
	}
	if st, err := os.Stat(path + "." + digest[:16] + ".part"); err != nil || st.Size() != 100 {
		t.Fatalf("cancelled download lost its progress: %v", err)
	}
}

func TestSourceIgnoringRangeRestartsFromTheBeginning(t *testing.T) {
	a, content, digest, path := resumeFixture(t)
	if err := os.WriteFile(path+"."+digest[:16]+".part", []byte(content[:100]), 0600); err != nil {
		t.Fatal(err)
	}
	if err := download(context.Background(), responseClient(content, 200), a, digest, path, func(int64) {}, func() {}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != content {
		t.Fatal("restarted archive differs")
	}
}
