package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/hostbridge"
)

func installSettingsPlugin(t *testing.T, s *Server, kind string) hostbridge.Manifest {
	t.Helper()
	dir := filepath.Join(s.hostPluginDir(), "notes")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"notes","version":"1","description":"A plugin.","permissions":[],"engines":{"cage":">=0.2.0"},"kind":"` + kind + `",
  "contributes":{"mcpServers":[{"id":"notes","command":["notes"]}],"settings":[
    {"id":"url","title":"URL","type":"string","env":"NOTES_URL","required":true},
    {"id":"token","title":"Token","type":"secret","env":"NOTES_TOKEN","required":true},
    {"id":"extra","title":"Extra","type":"secret","env":"NOTES_EXTRA"},
    {"id":"debug","title":"Debug","type":"boolean","env":"NOTES_DEBUG"},
    {"id":"mode","title":"Mode","type":"choice","env":"NOTES_MODE","choices":["a","b"],"default":"a"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := hostbridge.LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func settingText(value string) *string { return &value }

func TestPluginSettingsAreKeptAndGiven(t *testing.T) {
	s := testServer(t)
	manifest := installSettingsPlugin(t, s, hostbridge.KindHost)
	ctx := context.Background()
	_, err := s.pluginSettingsEnv(ctx, manifest)
	if missing, ok := errors.AsType[*hostbridge.MissingSettingsError](err); !ok || !slices.Equal(missing.Settings, []string{"URL", "Token"}) {
		t.Fatalf("before any settings: %v", err)
	}
	if len(pluginMissingSettings(manifest, s.readPluginSettings()["notes"])) != 2 {
		t.Fatal("missing settings are not judged without the secrets")
	}
	for name, values := range map[string]map[string]*string{
		"unknown id":     {"other": settingText("x")},
		"line break":     {"url": settingText("a\nb")},
		"choice":         {"mode": settingText("c")},
		"boolean":        {"debug": settingText("yes")},
		"too long value": {"token": settingText(strings.Repeat("a", hostbridge.SettingValueLimit+1))},
	} {
		if err := s.savePluginSettings("notes", values, false); err == nil {
			t.Errorf("%s: saved", name)
		}
	}
	if err := s.savePluginSettings("notes", map[string]*string{"url": settingText("https://example.com"), "token": settingText("tok'en $1"), "debug": settingText("true")}, false); err != nil {
		t.Fatal(err)
	}
	env, err := s.pluginSettingsEnv(ctx, manifest)
	want := []string{"NOTES_URL=https://example.com", "NOTES_TOKEN=tok'en $1", "NOTES_DEBUG=true", "NOTES_MODE=a"}
	if err != nil || !slices.Equal(env, want) {
		t.Fatalf("env %q: %v", env, err)
	}
	// The secret is stored sealed, and never shown.
	for _, name := range []string{pluginSettingsFile, pluginSecretsFile} {
		data, err := os.ReadFile(filepath.Join(s.profile, name))
		if err != nil || strings.Contains(string(data), "tok'en") {
			t.Fatalf("%s holds the secret in the clear: %v", name, err)
		}
	}
	view, _ := json.Marshal(s.hostToolsView())
	if strings.Contains(string(view), "tok'en") || !strings.Contains(string(view), `"id":"token","title":"Token","type":"secret","required":true,"set":true`) {
		t.Fatalf("view %s", view)
	}
	if !s.pluginUsesSecrets(manifest) {
		t.Fatal("a plugin that receives a secret is not stopped on lock")
	}
	// An empty secret keeps the one there; null clears it.
	if err := s.savePluginSettings("notes", map[string]*string{"token": settingText("")}, false); err != nil {
		t.Fatal(err)
	}
	if env, _ := s.pluginSettingsEnv(ctx, manifest); !slices.Contains(env, "NOTES_TOKEN=tok'en $1") {
		t.Fatalf("an empty value replaced the secret: %q", env)
	}
	if err := s.savePluginSettings("notes", map[string]*string{"token": nil, "debug": nil}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pluginSettingsEnv(ctx, manifest); err == nil || s.pluginUsesSecrets(manifest) {
		t.Fatalf("a cleared secret is still given: %v", err)
	}
}

func TestPluginSecretsNeedCageUnlocked(t *testing.T) {
	s := testServer(t)
	manifest := installSettingsPlugin(t, s, hostbridge.KindHost)
	if err := s.savePluginSettings("notes", map[string]*string{"url": settingText("u"), "token": settingText("one"), "extra": settingText("two")}, false); err != nil {
		t.Fatal(err)
	}
	s.account.lock()
	if _, err := s.pluginSettingsEnv(context.Background(), manifest); !errors.Is(err, errPluginSecretsLocked) {
		t.Fatalf("locked: %v", err)
	}
	if err := s.savePluginSettings("notes", map[string]*string{"token": settingText("three")}, false); err == nil {
		t.Fatal("saved a secret while locked")
	}
	// Plain values can still change.
	if err := s.savePluginSettings("notes", map[string]*string{"url": settingText("v")}, false); err != nil {
		t.Fatal(err)
	}
	// Removed while locked, the secrets stay sealed but are not given to a
	// plugin of the same name installed later.
	s.forgetPluginSettings("notes")
	s.account.setKey(make([]byte, 32))
	if err := s.savePluginSettings("notes", map[string]*string{"url": settingText("u"), "token": settingText("four")}, false); err != nil {
		t.Fatal(err)
	}
	env, err := s.pluginSettingsEnv(context.Background(), manifest)
	if err != nil || !slices.Contains(env, "NOTES_TOKEN=four") || slices.ContainsFunc(env, func(entry string) bool { return strings.HasPrefix(entry, "NOTES_EXTRA=") }) {
		t.Fatalf("env %q: %v", env, err)
	}
	s.forgetPluginSettings("notes")
	if _, err := os.Stat(filepath.Join(s.profile, pluginSettingsFile)); err != nil {
		t.Fatal(err)
	}
	if len(s.readPluginSettings()) != 0 {
		t.Fatal("settings kept after removal")
	}
	_ = s.account.withPluginSecrets(false, func(all map[string]map[string]string) error {
		if len(all) != 0 {
			t.Errorf("secrets kept after removal: %d", len(all))
		}
		return nil
	})
}

func TestGuestPluginSecretsNeedConsent(t *testing.T) {
	s := testServer(t)
	manifest := installSettingsPlugin(t, s, hostbridge.KindGuest)
	values := map[string]*string{"url": settingText("u"), "token": settingText("one")}
	if err := s.savePluginSettings("notes", values, false); err == nil {
		t.Fatal("a secret was given to a plugin inside the environment without consent")
	}
	if _, err := os.Stat(filepath.Join(s.profile, pluginSecretsFile)); !os.IsNotExist(err) {
		t.Fatal("the secret was stored without consent")
	}
	if err := s.savePluginSettings("notes", values, true); err != nil {
		t.Fatal(err)
	}
	if env, err := s.pluginSettingsEnv(context.Background(), manifest); err != nil || !slices.Contains(env, "NOTES_TOKEN=one") {
		t.Fatalf("env %q: %v", env, err)
	}
	// A secret kept for the plugin when it ran on this computer is not
	// given once it runs inside the environment.
	record := s.readPluginSettings()["notes"]
	record.GuestSecrets = false
	if missing := pluginMissingSettings(manifest, record); !slices.Equal(missing, []string{"Token"}) {
		t.Fatalf("missing %q", missing)
	}
	if view := pluginSettingsView(manifest, record); view[1].Set {
		t.Fatal("a secret not given shows as set")
	}
}

func TestSetHostToolsSavesPluginSettings(t *testing.T) {
	s := testServer(t)
	installSettingsPlugin(t, s, hostbridge.KindHost)
	view := s.hostToolsView()
	index := slices.IndexFunc(view.Plugins, func(plugin hostPluginView) bool { return plugin.Name == "notes" })
	if index < 0 || !view.Plugins[index].NeedsSettings || len(view.Plugins[index].Settings) != 5 {
		t.Fatalf("view %+v", view.Plugins)
	}
	if err := s.savePluginSettings("notes", map[string]*string{"url": settingText("u"), "token": settingText("one")}, false); err != nil {
		t.Fatal(err)
	}
	view = s.hostToolsView()
	if view.Plugins[index].NeedsSettings {
		t.Fatal("still needs settings")
	}
	if err := s.savePluginSettings("../notes", map[string]*string{"url": settingText("u")}, false); err == nil {
		t.Fatal("saved for a path")
	}
}

// The settings file is read by the run script once, so the values reach the
// program and nothing else, and is gone after.
func TestGuestPluginSettingsReachTheProgram(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script runs in the environment, which is Linux")
	}
	home, folder := t.TempDir(), ".cage-plugins/notes"
	if err := os.MkdirAll(filepath.Join(home, folder), 0o700); err != nil {
		t.Fatal(err)
	}
	value := `it's "$HOME" ` + "`x` \\ ;"
	write := exec.Command("sh", "-c", guestPluginSettingsScript, "sh", folder)
	write.Env = append(os.Environ(), "HOME="+home)
	write.Stdin = strings.NewReader(guestPluginSettingsFile([]string{"NOTES_TOKEN=" + value, "NOTES_URL=u"}))
	if output, err := write.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, output)
	}
	if info, err := os.Stat(filepath.Join(home, folder+".env")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("settings file: %v %v", info, err)
	}
	run := exec.Command("sh", "-c", guestPluginRunScript, "sh", folder, "1", "env")
	run.Env = append(os.Environ(), "HOME="+home)
	output, err := run.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"NOTES_TOKEN=" + value, "NOTES_URL=u"} {
		if !strings.Contains("\n"+string(output), "\n"+line+"\n") {
			t.Fatalf("%s is missing:\n%s", line, output)
		}
	}
	if _, err := os.Stat(filepath.Join(home, folder+".env")); !os.IsNotExist(err) {
		t.Fatal("the settings file was left behind")
	}
}
