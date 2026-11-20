package uilocale

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/sylvinus/agent-vm/internal/host"
)

// SeedWindowsInstallerChoice imports an explicit installer selection without
// replacing any existing application preference, including system mode.
func SeedWindowsInstallerChoice(profile string, id int) error {
	var languages []struct {
		ID                 string
		WindowsInstallerID int `json:"windowsInstallerId"`
	}
	if err := json.Unmarshal(registry, &languages); err != nil {
		return err
	}
	for _, language := range languages {
		if language.WindowsInstallerID == id {
			return seed(profile, Preference{Language: language.ID})
		}
	}
	return errors.New("unsupported installer language")
}

func seed(profile string, pref Preference) error {
	path := filepath.Join(profile, PreferenceFile)
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := host.EnsurePrivateDir(profile); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = host.ProtectFile(path); err == nil {
		err = json.NewEncoder(file).Encode(pref)
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}
