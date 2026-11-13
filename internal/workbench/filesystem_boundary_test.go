package workbench

import "testing"

func TestFilesystemBoundaryWithAndWithoutProjects(t *testing.T) {
	for _, test := range []struct {
		name, private, data              string
		layout, shared, noProjects, want bool
	}{
		{"no projects", "unreachable", "nonce", true, false, true, true},
		{"unexpected share", "unreachable", "nonce:guest", true, true, true, false},
		{"unexpected write", "unreachable", "changed", true, false, true, false},
		{"private readable", "readable", "nonce", true, false, true, false},
		{"private unknown", "unknown", "nonce", true, false, true, false},
		{"unexpected mount", "unreachable", "nonce", false, false, true, false},
		{"project round trip", "unreachable", "nonce:guest", true, true, false, true},
		{"project not writable", "unreachable", "nonce", true, true, false, false},
		{"project observation absent", "unreachable", "nonce:guest", true, false, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			boundary := guestObservation{PrivateFile: test.private, SharedFile: test.shared}
			if got := filesystemBoundaryVerified(test.layout, boundary, test.data, "nonce", test.noProjects); got != test.want {
				t.Fatalf("boundary result=%t want=%t", got, test.want)
			}
		})
	}
}
