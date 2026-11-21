package workbench

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sylvinus/agent-vm/internal/host"
	"github.com/sylvinus/agent-vm/internal/hostbridge"
)

// A plugin's settings are kept in two files of the profile. What may be read
// while Cage is locked is in plugin-settings.json: plain values, which
// secrets are set, and whether the user agreed that a plugin inside the
// environment receives its secrets. The secrets themselves are in
// plugin-secrets.json, sealed with a key derived from the account key as the
// password vault is, so they are readable only while Cage is unlocked.
const (
	pluginSettingsFile  = "plugin-settings.json"
	pluginSecretsFile   = "plugin-secrets.json"
	pluginSecretsDomain = "Cage plugin secrets v1"
	pluginSecretsLimit  = 1 << 20
)

// errPluginSecretsLocked is a plugin that cannot receive its secrets while
// Cage is locked.
var errPluginSecretsLocked = errors.New("Cage 已锁定，解锁后插件会重新启动")

type pluginSettingsRecord struct {
	Values map[string]string `json:"values,omitempty"`
	// Secrets are the ids of the secrets that have a value.
	Secrets []string `json:"secrets,omitempty"`
	// GuestSecrets says the user agreed that the plugin, running inside the
	// environment, receives its secrets there.
	GuestSecrets bool `json:"guest_secrets,omitempty"`
}

type pluginSettingsData struct {
	Version int                             `json:"version"`
	Plugins map[string]pluginSettingsRecord `json:"plugins"`
}

func (s *Server) readPluginSettings() map[string]pluginSettingsRecord {
	plugins := map[string]pluginSettingsRecord{}
	data, err := os.ReadFile(filepath.Join(s.profile, pluginSettingsFile))
	if err != nil {
		return plugins
	}
	var file pluginSettingsData
	// A file that cannot be read gives no values, so required settings ask again.
	if json.Unmarshal(data, &file) == nil && file.Plugins != nil {
		plugins = file.Plugins
	}
	return plugins
}

func (s *Server) writePluginSettings(plugins map[string]pluginSettingsRecord) error {
	return writePrivate(filepath.Join(s.profile, pluginSettingsFile), pluginSettingsData{1, plugins})
}

// withPluginSecrets reads, and with mutate writes back, the sealed secrets of
// every plugin, by plugin name and then setting id.
func (a *localAccount) withPluginSecrets(mutate bool, fn func(map[string]map[string]string) error) error {
	if a == nil {
		return errPluginSecretsLocked
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.key) != 32 {
		return errPluginSecretsLocked
	}
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(pluginSecretsDomain))
	key := mac.Sum(nil)
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(a.path), pluginSecretsFile)
	secrets := map[string]map[string]string{}
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return errors.New("无法读取插件密钥")
	}
	if err == nil {
		if host.CheckPrivateFile(path) != nil {
			return errors.New("插件密钥文件权限不安全")
		}
		var sealed vaultEnvelope
		if len(raw) > pluginSecretsLimit*2 || json.Unmarshal(raw, &sealed) != nil || sealed.Version != 1 || len(sealed.Nonce) != aead.NonceSize() {
			return errors.New("插件密钥文件损坏；现有文件未修改")
		}
		plain, err := aead.Open(nil, sealed.Nonce, sealed.Data, []byte(pluginSecretsDomain))
		if err != nil {
			return errors.New("插件密钥无法解密；现有文件未修改")
		}
		defer clear(plain)
		if len(plain) > pluginSecretsLimit || json.Unmarshal(plain, &secrets) != nil {
			return errors.New("插件密钥内容无效；现有文件未修改")
		}
		if secrets == nil {
			secrets = map[string]map[string]string{}
		}
	}
	if err = fn(secrets); err != nil || !mutate {
		return err
	}
	plain, err := json.Marshal(secrets)
	if err != nil {
		return err
	}
	defer clear(plain)
	if len(plain) > pluginSecretsLimit {
		return errors.New("插件密钥过多，请删除不再使用的密钥")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	if err = writePrivate(path, vaultEnvelope{1, nonce, aead.Seal(nil, nonce, plain, []byte(pluginSecretsDomain))}); err != nil {
		return errors.New("插件密钥未能保存，请检查磁盘空间与权限")
	}
	return nil
}

func pluginSettingsOf(manifest hostbridge.Manifest) []hostbridge.Setting {
	if manifest.Contributes == nil {
		return nil
	}
	return manifest.Contributes.Settings
}

// pluginSecretsGiven are the secrets of the plugin its program receives:
// those that are set, and for a plugin inside the environment only once
// the user agreed to it.
func pluginSecretsGiven(manifest hostbridge.Manifest, record pluginSettingsRecord) []string {
	if manifest.Kind == hostbridge.KindGuest && !record.GuestSecrets {
		return nil
	}
	var given []string
	for _, setting := range pluginSettingsOf(manifest) {
		if setting.Type == hostbridge.SettingSecret && slices.Contains(record.Secrets, setting.ID) {
			given = append(given, setting.ID)
		}
	}
	return given
}

// pluginSettingValue is the value of a setting that is not a secret: the
// user's, else the default. Values a newer manifest no longer allows fall
// back to the default too.
func pluginSettingValue(setting hostbridge.Setting, record pluginSettingsRecord) string {
	value, set := record.Values[setting.ID]
	switch setting.Type {
	case hostbridge.SettingBoolean:
		if value != "true" && value != "false" {
			value = setting.Default
		}
		if value == "" {
			value = "false"
		}
		return value
	case hostbridge.SettingChoice:
		if !slices.Contains(setting.Choices, value) {
			value = setting.Default
		}
		return value
	}
	if !set || value == "" {
		value = setting.Default
	}
	return value
}

// pluginMissingSettings are the titles of the required settings that have
// no value, judged without reading the secrets.
func pluginMissingSettings(manifest hostbridge.Manifest, record pluginSettingsRecord) []string {
	given := pluginSecretsGiven(manifest, record)
	var missing []string
	for _, setting := range pluginSettingsOf(manifest) {
		if !setting.Required {
			continue
		}
		if setting.Type == hostbridge.SettingSecret && !slices.Contains(given, setting.ID) ||
			setting.Type != hostbridge.SettingSecret && pluginSettingValue(setting, record) == "" {
			missing = append(missing, setting.Title)
		}
	}
	return missing
}

// pluginSettingsEnv gives the variables of the plugin's settings for its
// program. It fails with a MissingSettingsError while a required setting has
// no value, and with errPluginSecretsLocked while Cage is locked and the
// plugin has secrets to receive.
func (s *Server) pluginSettingsEnv(_ context.Context, manifest hostbridge.Manifest) ([]string, error) {
	settings := pluginSettingsOf(manifest)
	if len(settings) == 0 {
		return nil, nil
	}
	s.pluginSettingsMu.Lock()
	record := s.readPluginSettings()[manifest.Name]
	s.pluginSettingsMu.Unlock()
	if missing := pluginMissingSettings(manifest, record); len(missing) > 0 {
		return nil, &hostbridge.MissingSettingsError{Plugin: manifest.Name, Settings: missing}
	}
	given := pluginSecretsGiven(manifest, record)
	secrets := map[string]string{}
	if len(given) > 0 {
		err := s.account.withPluginSecrets(false, func(all map[string]map[string]string) error {
			for _, id := range given {
				if value := all[manifest.Name][id]; value != "" {
					secrets[id] = value
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	var env, missing []string
	for _, setting := range settings {
		value := secrets[setting.ID]
		if setting.Type != hostbridge.SettingSecret {
			value = pluginSettingValue(setting, record)
		}
		if value == "" {
			if setting.Required {
				missing = append(missing, setting.Title)
			}
			continue
		}
		env = append(env, setting.Env+"="+value)
	}
	if len(missing) > 0 {
		return nil, &hostbridge.MissingSettingsError{Plugin: manifest.Name, Settings: missing}
	}
	return env, nil
}

// pluginUsesSecrets reports whether the plugin's program receives secrets,
// and so must stop when Cage locks.
func (s *Server) pluginUsesSecrets(manifest hostbridge.Manifest) bool {
	if len(pluginSettingsOf(manifest)) == 0 {
		return false
	}
	s.pluginSettingsMu.Lock()
	defer s.pluginSettingsMu.Unlock()
	return len(pluginSecretsGiven(manifest, s.readPluginSettings()[manifest.Name])) > 0
}

// pluginSettingsMissingMessage tells the user what to fill in.
func pluginSettingsMissingMessage(titles []string) string {
	return "请先在插件的「设置」中填写：" + strings.Join(titles, "、")
}

// savePluginSettings records the values the user entered. A value of nil
// clears a setting; an empty secret is left as it is, since secrets are
// never shown to be edited. Giving a plugin inside the environment a secret
// needs confirmGuest: the secret then leaves this computer.
func (s *Server) savePluginSettings(name string, values map[string]*string, confirmGuest bool) error {
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return errors.New("插件名称无效")
	}
	manifest, err := hostbridge.LoadManifest(filepath.Join(s.hostPluginDir(), name))
	if err != nil || manifest.Name != name {
		return errors.New("没有找到这个插件")
	}
	settings := pluginSettingsOf(manifest)
	if len(settings) == 0 {
		return errors.New("这个插件没有可填写的设置")
	}
	s.pluginSettingsMu.Lock()
	defer s.pluginSettingsMu.Unlock()
	plugins := s.readPluginSettings()
	record := plugins[name]
	next := map[string]string{}
	for id, value := range record.Values {
		next[id] = value
	}
	secrets := map[string]*string{}
	for id, value := range values {
		index := slices.IndexFunc(settings, func(setting hostbridge.Setting) bool { return setting.ID == id })
		if index < 0 {
			return fmt.Errorf("没有设置项 %s", id)
		}
		setting := settings[index]
		if value != nil && !hostbridge.ValidSettingValue(*value) {
			return fmt.Errorf("「%s」过长或含有换行", setting.Title)
		}
		switch {
		case setting.Type == hostbridge.SettingSecret:
			if value == nil || *value != "" {
				secrets[id] = value
			}
		case value == nil || *value == "":
			delete(next, id)
		case setting.Type == hostbridge.SettingBoolean && *value != "true" && *value != "false":
			return fmt.Errorf("「%s」只能是开或关", setting.Title)
		case setting.Type == hostbridge.SettingChoice && !slices.Contains(setting.Choices, *value):
			return fmt.Errorf("「%s」不是可选的值", setting.Title)
		default:
			next[id] = *value
		}
	}
	giving := false
	for _, value := range secrets {
		giving = giving || value != nil
	}
	if giving && manifest.Kind == hostbridge.KindGuest && !confirmGuest {
		return errors.New("把密钥交给环境内的插件前，请先确认风险")
	}
	if len(secrets) > 0 {
		err = s.account.withPluginSecrets(true, func(all map[string]map[string]string) error {
			mine := map[string]string{}
			// Only secrets the record knows are kept: others are left from a
			// plugin of this name removed while Cage was locked.
			for id, value := range all[name] {
				if slices.Contains(record.Secrets, id) {
					mine[id] = value
				}
			}
			for id, value := range secrets {
				if value == nil {
					delete(mine, id)
				} else {
					mine[id] = *value
				}
			}
			if len(mine) == 0 {
				delete(all, name)
			} else {
				all[name] = mine
			}
			record.Secrets = record.Secrets[:0:0]
			for _, setting := range settings {
				if _, set := mine[setting.ID]; set && setting.Type == hostbridge.SettingSecret {
					record.Secrets = append(record.Secrets, setting.ID)
				}
			}
			return nil
		})
		if errors.Is(err, errPluginSecretsLocked) {
			return errors.New("请先解锁 Cage")
		}
		if err != nil {
			return err
		}
	}
	if giving && manifest.Kind == hostbridge.KindGuest {
		record.GuestSecrets = true
	}
	if len(record.Secrets) == 0 {
		record.GuestSecrets = false
	}
	record.Values = next
	if len(record.Values) == 0 && len(record.Secrets) == 0 {
		delete(plugins, name)
	} else {
		plugins[name] = record
	}
	return s.writePluginSettings(plugins)
}

// forgetPluginSettings removes what the user entered for a plugin that is
// uninstalled. Secrets stay sealed while Cage is locked and are removed the
// next time the plugin's settings are saved.
func (s *Server) forgetPluginSettings(name string) {
	s.pluginSettingsMu.Lock()
	defer s.pluginSettingsMu.Unlock()
	plugins := s.readPluginSettings()
	if _, found := plugins[name]; found {
		delete(plugins, name)
		_ = s.writePluginSettings(plugins)
	}
	_ = s.account.withPluginSecrets(true, func(all map[string]map[string]string) error {
		delete(all, name)
		return nil
	})
}

// hostSettingView is one setting as the plugin page shows it. A secret's
// value is never sent; Set says whether it has one.
type hostSettingView struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type"`
	Required    bool     `json:"required,omitempty"`
	Default     string   `json:"default,omitempty"`
	Choices     []string `json:"choices,omitempty"`
	Value       string   `json:"value,omitempty"`
	Set         bool     `json:"set,omitempty"`
}

func pluginSettingsView(manifest hostbridge.Manifest, record pluginSettingsRecord) []hostSettingView {
	given := pluginSecretsGiven(manifest, record)
	var views []hostSettingView
	for _, setting := range pluginSettingsOf(manifest) {
		view := hostSettingView{ID: setting.ID, Title: setting.Title, Description: setting.Description, Type: setting.Type,
			Required: setting.Required, Default: setting.Default, Choices: setting.Choices}
		if setting.Type == hostbridge.SettingSecret {
			// One kept from before the plugin moved inside the environment is
			// not given to it, so it shows as unset until entered again.
			view.Set = slices.Contains(given, setting.ID)
		} else {
			view.Value = record.Values[setting.ID]
		}
		views = append(views, view)
	}
	return views
}
