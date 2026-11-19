package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImplementationLookupIncludesReturnAfterChunkBoundary(t *testing.T) {
	name := filepath.Join(t.TempDir(), "source.tar.gz")
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	archive := tar.NewWriter(gz)
	files := map[string]string{
		"source/docs/guide/update.md":        strings.Repeat("更新器遇到限流，请稍后重试。\n", 8),
		"source/internal/appupdate/retry.go": "func updateSourceError() {\n" + strings.Repeat("// Inspect response headers.\n", 39) + "return now.Add(5 * time.Minute) // fallback retry time\n}",
	}
	for path, data := range files {
		if err := archive.WriteHeader(&tar.Header{Name: path, Mode: 0644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	for _, closer := range []interface{ Close() error }{archive, gz, f} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	ix, err := loadIndex(name, "pinned")
	if err != nil {
		t.Fatal(err)
	}
	hits := ix.search("更新器 updateSourceError 遇到限流会多久后重试？请查看源码", "")
	if len(hits) < 2 || !strings.Contains(hits[0].Text, "updateSourceError") || !strings.Contains(hits[1].Text, "5 * time.Minute") || !strings.Contains(hits[1].URL, "#L41-") {
		t.Fatalf("missing the matched function or its fallback branch: %#v", hits)
	}
}

func TestOnlyExplicitOwnerPolicyCanEnterFeishu(t *testing.T) {
	d := decision{Status: "knowledge_gap", Policy: "service_commitment", Question: "是否提供预约指导？"}
	for _, topic := range []string{"technical", "", "diagnose", "roadmap"} {
		if (lookupPlan{Topic: topic}).canAskOwner(d) {
			t.Fatalf("escalated topic %q", topic)
		}
	}
	if !(lookupPlan{Topic: "service_commitment"}).canAskOwner(d) {
		t.Fatal("lost actionable policy request")
	}
	d.Status = "answered"
	if (lookupPlan{Topic: "service_commitment"}).canAskOwner(d) {
		t.Fatal("escalated an answered question")
	}
}

func TestProviderOutageQuotesOnlyRelevantProjectText(t *testing.T) {
	refs := []source{
		{Title: "项目维护者答复", Text: "可以为企业用户提供指导。"},
		{Title: "打开项目", URL: "https://claude-cage.com/docs/workbench/", Text: "在项目选择窗口选中项目，点击编辑名称与文件夹，可以添加或解绑文件夹。"},
	}
	got := referenceOnly("项目选择窗口能否解绑文件夹？", refs)
	if !strings.Contains(got, refs[1].Text) || !strings.Contains(got, "[2]") {
		t.Fatal(got)
	}
	if strings.Contains(referenceOnly("Magpie", refs), refs[1].Text) {
		t.Fatal("unrelated manual excerpt")
	}
}
