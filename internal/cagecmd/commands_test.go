package cagecmd

import "testing"

func TestAccountNameCannotHideHostCommandOrExistingSelector(t *testing.T) {
	for _, name := range []string{"start", "ClaUDe", "AuTo", "app-update", "hostagent", "1234567890ABCDEF12345678", "03", "１２", "-p", "./test", "a@b.com", "a b", "a\x1bb", "x-\u0301"} {
		if ValidAccountName(name) {
			t.Errorf("accepted unavailable account name %q", name)
		}
	}
	for _, name := range []string{"Sarah", "3d-work", "工作.一", "de_v", "e\u0301quipe"} {
		if !ValidAccountName(name) {
			t.Errorf("rejected account name %q", name)
		}
	}
	if AccountNameKey("E\u0301quipe") != AccountNameKey("éQUIPE") {
		t.Fatal("canonically equivalent account names have different keys")
	}
}
