package guestprofile

import (
	"reflect"
	"testing"
)

func TestHardwareBackendsSelectANGLEAndZeroCopy(t *testing.T) {
	for _, tc := range []struct {
		backend string
		angle   string
	}{
		{"metal", "metal"},
		{"d3d11", "d3d11"},
		{"vulkan", "vulkan"},
		{"opengl", "gl"},
	} {
		got := (&Profile{Options: Options{GraphicsBackend: tc.backend}}).GraphicsFlags()
		want := []string{"--use-gl=angle", "--use-angle=" + tc.angle, "--enable-zero-copy"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s flags = %v, want %v", tc.backend, got, want)
		}
	}
}

func TestAutomaticAndSoftwareGraphicsDoNotForceGPUFlags(t *testing.T) {
	for _, p := range []*Profile{nil, {}, {Options: Options{GraphicsBackend: "auto"}}} {
		if flags := p.GraphicsFlags(); len(flags) != 0 {
			t.Fatalf("automatic profile returned explicit flags: %v", flags)
		}
	}
	got := (&Profile{Options: Options{GraphicsBackend: "software"}}).GraphicsFlags()
	want := []string{"--use-gl=angle", "--use-angle=swiftshader"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("software flags = %v, want %v", got, want)
	}
}
