package vm

import (
	"context"
	"testing"
)

func TestFirstBootMarkerOnlyOnSetupStart(t *testing.T) {
	if firstBoot(context.Background()) {
		t.Fatal("ordinary starts must keep the fixed timeout")
	}
	if !firstBoot(WithFirstBoot(context.Background())) {
		t.Fatal("setup start lost the first-boot marker")
	}
	if FirstBootIdleWait <= 0 || FirstBootMaxWait <= FirstBootIdleWait {
		t.Fatal("first boot needs an idle bound inside a longer overall bound")
	}
}
