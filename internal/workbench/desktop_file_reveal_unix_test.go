//go:build !windows

package workbench

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestDesktopFileRevealCommandsPreserveSelection(t *testing.T) {
	path := "/tmp/项目/Word #+%; $(touch unsafe).docx"
	ctx := context.Background()
	if args := hostRevealCommand(ctx, "darwin", path).Args; !reflect.DeepEqual(args, []string{"/usr/bin/open", "-R", "--", path}) {
		t.Fatal("macOS reveal lost selection", args)
	}
	args := hostRevealCommand(ctx, "linux", path).Args
	if args[0] != "gdbus" || !strings.Contains(strings.Join(args, " "), "ShowItems") || !strings.Contains(args[len(args)-2], "%23+%25") {
		t.Fatal("Linux reveal lost selection or URI escaping", args)
	}
}
