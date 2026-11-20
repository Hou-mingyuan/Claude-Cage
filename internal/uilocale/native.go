package uilocale

import (
	_ "embed"
	"encoding/json"
)

//go:embed native.json
var nativeCatalog []byte

type NativeLanguage struct {
	Active   string            `json:"active"`
	Messages map[string]string `json:"messages"`
}

func Native(profile string) (NativeLanguage, error) {
	active := Load(profile).Active(Host())
	var catalogs map[string]json.RawMessage
	if err := json.Unmarshal(nativeCatalog, &catalogs); err != nil {
		return NativeLanguage{}, err
	}
	messages := make(map[string]string)
	if data, ok := catalogs[active]; ok {
		if err := json.Unmarshal(data, &messages); err != nil {
			return NativeLanguage{}, err
		}
	}
	return NativeLanguage{active, messages}, nil
}
