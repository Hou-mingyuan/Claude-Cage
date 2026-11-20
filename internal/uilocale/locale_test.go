package uilocale

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeCatalogMatchesSourceKeys(t *testing.T) {
	var catalogs map[string]map[string]string
	if err := json.Unmarshal(nativeCatalog, &catalogs); err != nil {
		t.Fatal(err)
	}
	var reference map[string]string
	for language, messages := range catalogs {
		if reference == nil {
			reference = messages
		}
		if len(messages) != len(reference) {
			t.Errorf("%s has %d native messages, want %d", language, len(messages), len(reference))
		}
		for source, text := range messages {
			if _, ok := reference[source]; !ok {
				t.Errorf("%s has an unexpected native message %q", language, source)
			}
			if strings.TrimSpace(text) == "" || strings.Count(text, "{0}") != strings.Count(source, "{0}") {
				t.Errorf("%s native message %q has a missing translation or placeholder", language, source)
			}
		}
	}
}

func TestMatchHostDisplayLanguages(t *testing.T) {
	tests := []struct {
		preferred []string
		want      string
	}{
		{nil, "zh-CN"},
		{[]string{"en-US", "de-DE"}, "zh-CN"},
		{[]string{"de-DE", "fr-CA", "ko-KR"}, "fr"},
		{[]string{"ru_RU.UTF-8"}, "ru"},
		{[]string{"zh-Hant-CN"}, "zh-TW"},
		{[]string{"zh-Hans-TW"}, "zh-CN"},
		{[]string{"zh-HK"}, "zh-TW"},
		{[]string{"zh_SG"}, "zh-CN"},
		{[]string{"fa-AF"}, "prs"},
		{[]string{"fa-IR"}, "fa"},
		{[]string{"prs-Arab-AF"}, "prs"},
		{[]string{"ps_PK.UTF-8"}, "ps"},
		{[]string{"ti_ER.UTF-8@saaho"}, "ti"},
		{[]string{"my-MM"}, "my"},
	}
	for _, tc := range tests {
		if got := Match(tc.preferred); got != tc.want {
			t.Errorf("Match(%q) = %q, want %q", tc.preferred, got, tc.want)
		}
	}
}
