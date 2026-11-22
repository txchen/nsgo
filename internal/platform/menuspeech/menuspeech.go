// Package menuspeech shares the desktop Speech menu's asynchronous selection
// request ordering and the expression that reads a web page's selection.
package menuspeech

import "encoding/json"

// State orders requests on the backend's UI thread. The most recent Start or
// Stop wins across windows, as the menu's one utterance does.
type State struct{ serial uint64 }

// Request is one Start Speaking action waiting for its selection.
type Request struct {
	state  *State
	serial uint64
}

// Begin supersedes any earlier selection request.
func (s *State) Begin() Request {
	s.serial++
	return Request{s, s.serial}
}

// Cancel prevents a pending selection from starting speech later.
func (s *State) Cancel() { s.serial++ }

// Take accepts the current request once, including when Request was copied.
func (r Request) Take() bool {
	if r.state == nil || r.state.serial != r.serial {
		return false
	}
	r.state.Cancel()
	return true
}

// WebSelection reads a range from the focused document, including a text form
// control and same-origin child frames. It returns nothing for a password or a
// document the top frame cannot access, and never substitutes the page's body.
const WebSelection = `(function () {
  var page = window;
  for (;;) {
    var field = page.document.activeElement;
    if (field && (field.tagName === "IFRAME" || field.tagName === "FRAME")) {
      try {
        var child = field.contentWindow;
        if (!child || child === page || !child.document) return "";
        page = child;
        continue;
      } catch (_) { return ""; }
    }
    if (field && (field.tagName === "INPUT" || field.tagName === "TEXTAREA")) {
      if (field.type === "password") return "";
      var start = field.selectionStart, end = field.selectionEnd;
      if (typeof start !== "number" || typeof end !== "number") return "";
      return String(field.value).slice(Math.min(start, end), Math.max(start, end));
    }
    var range = page.getSelection();
    return range ? range.toString() : "";
  }
})()`

// WebSelectionObject returns a marker and selected text for an asynchronous
// native callback whose API has no request token.
func WebSelectionObject(marker string) string {
	return `({ marker: ` + quoteJS(marker) + `, text: (` + WebSelection + `) })`
}

func quoteJS(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
