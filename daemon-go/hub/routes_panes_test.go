package hub

import (
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

func TestLocalPaneIdentityUsesConfiguredBoundary(t *testing.T) {
	pane := &localPane{Session: "mesh", Window: "work", WindowID: "@7"}
	if circle, locator := localPaneIdentity(proto.CircleBoundarySession, pane); circle != "mesh" || locator != "mesh:work" {
		t.Fatalf("session identity = %q, %q", circle, locator)
	}
	if circle, locator := localPaneIdentity(proto.CircleBoundaryWindow, pane); circle != "window-7" || locator != "mesh:work" {
		t.Fatalf("window identity = %q, %q", circle, locator)
	}
}
