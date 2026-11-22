package menuspeech

import (
	"strings"
	"testing"
)

func TestSelectionRepliesFollowMenuActions(t *testing.T) {
	var s State
	a := s.Begin()
	b := s.Begin()
	if a.Take() {
		t.Fatal("an older reply won after a newer request")
	}
	if !b.Take() {
		t.Fatal("the current reply was dropped")
	}
	if b.Take() {
		t.Fatal("a completion was accepted twice")
	}
	c := s.Begin()
	s.Cancel()
	if c.Take() {
		t.Fatal("Stop Speaking did not cancel the pending selection")
	}
}

func TestWebSelectionScriptReadsFocusedControlsAndFrames(t *testing.T) {
	for _, want := range []string{"activeElement", "selectionStart", "TEXTAREA", "IFRAME", "password", "getSelection"} {
		if !strings.Contains(WebSelection, want) {
			t.Fatalf("selection script lacks %q", want)
		}
	}
	if !strings.Contains(WebSelectionObject("marker"), "marker") {
		t.Fatal("object script lost its marker")
	}
}
