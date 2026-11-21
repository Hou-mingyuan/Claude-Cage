package hostbridge

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const settingsManifest = `{"name":"notes","version":"1","description":"A plugin.","permissions":[],"engines":{"cage":">=0.2.0"},"kind":"host",
  "contributes":{"mcpServers":[{"id":"notes","command":["notes"]}],"settings":[
    {"id":"url","title":"URL","type":"string","env":"NOTES_URL","required":true},
    {"id":"token","title":"Token","type":"secret","env":"NOTES_TOKEN"},
    {"id":"debug","title":"Debug","type":"boolean","env":"NOTES_DEBUG","default":"false"},
    {"id":"mode","title":"Mode","type":"choice","env":"NOTES_MODE","choices":["a","b"],"default":"a"}]}}`

func TestManifestSettings(t *testing.T) {
	m, err := parseManifest([]byte(settingsManifest))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Contributes.Settings) != 4 || m.Contributes.Settings[1].Type != SettingSecret {
		t.Fatalf("settings %+v", m.Contributes.Settings)
	}
	if _, err := parseManifest([]byte(strings.Replace(settingsManifest, `>=0.2.0`, `^0.3.1`, 1))); err != nil {
		t.Errorf("newer engine: %v", err)
	}
	broken := map[string][2]string{
		"engine before settings": {`>=0.2.0`, `>=0.1.9`},
		"caret before settings":  {`>=0.2.0`, `^0.1.0`},
		"id":                     {`"id":"url"`, `"id":"URL"`},
		"id repeated":            {`"id":"mode"`, `"id":"url"`},
		"env repeated":           {`"env":"NOTES_MODE"`, `"env":"NOTES_URL"`},
		"env lowercase":          {`"env":"NOTES_URL"`, `"env":"notes_url"`},
		"env reserved":           {`"env":"NOTES_URL"`, `"env":"PATH"`},
		"env loader":             {`"env":"NOTES_URL"`, `"env":"LD_PRELOAD"`},
		"type":                   {`"type":"string"`, `"type":"number"`},
		"title missing":          {`"title":"URL",`, ``},
		"secret default":         {`"type":"secret",`, `"type":"secret","default":"x",`},
		"boolean default":        {`"default":"false"`, `"default":"yes"`},
		"choice default":         {`"default":"a"`, `"default":"c"`},
		"choices empty":          {`"choices":["a","b"]`, `"choices":[]`},
		"choices repeated":       {`"choices":["a","b"]`, `"choices":["a","a"]`},
		"choices on a string":    {`"type":"string",`, `"type":"string","choices":["a"],`},
		"default with a break":   {`"required":true`, `"required":true,"default":"a\nb"`},
		"unknown field":          {`"required":true`, `"required":true,"secret":true`},
	}
	for name, change := range broken {
		if !strings.Contains(settingsManifest, change[0]) {
			t.Fatalf("%s: the example has no %q", name, change[0])
		}
		if _, err := parseManifest([]byte(strings.Replace(settingsManifest, change[0], change[1], 1))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if !ValidSettingValue("https://example.com/a b") || ValidSettingValue("a\nb") || ValidSettingValue("a\x00") || ValidSettingValue(strings.Repeat("a", SettingValueLimit+1)) {
		t.Error("ValidSettingValue")
	}
}

func TestWithEnvReplacesByName(t *testing.T) {
	got := withEnv([]string{"HOME=/h", "NOTES_URL=old", "Path=x"}, []string{"NOTES_URL=new", "NOTES_TOKEN=t", "PATH=y"})
	want := []string{"HOME=/h", "NOTES_URL=new", "NOTES_TOKEN=t", "PATH=y"}
	if runtime.GOOS != "windows" {
		want = []string{"HOME=/h", "Path=x", "NOTES_URL=new", "NOTES_TOKEN=t", "PATH=y"}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A plugin whose settings are missing is not started and says which.
func TestExternalWaitsForSettings(t *testing.T) {
	m, err := parseManifest([]byte(settingsManifest))
	if err != nil {
		t.Fatal(err)
	}
	plugin := &External{Manifest: m, Dir: t.TempDir(), Env: func(context.Context) ([]string, error) {
		return nil, &MissingSettingsError{Plugin: "notes", Settings: []string{"URL"}}
	}}
	err = plugin.Start(context.Background())
	if missing, ok := errors.AsType[*MissingSettingsError](err); !ok || !slices.Equal(missing.Settings, []string{"URL"}) {
		t.Fatalf("start: %v", err)
	}
	if plugin.Running() {
		t.Fatal("started without its settings")
	}
	failure := errors.New("locked")
	plugin.Env = func(context.Context) ([]string, error) { return nil, failure }
	if err := plugin.Start(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("start: %v", err)
	}
}
