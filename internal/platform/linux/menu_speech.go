//go:build linux && !android

package linux

import (
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/nsgo-dev/nsgo/internal/platform/menuspeech"

	"github.com/nsgo-dev/nsgo/internal/driver"
	"github.com/nsgo-dev/nsgo/internal/platform/speechd"
)

// menuUtterance numbers what the Speech menu's Start Speaking says. Core numbers its utterances from 1, so speech-dispatcher's utterance 0 is the menu's, whose end no engine hears.
const menuUtterance = 0

var webSpeechState menuspeech.State
var webSpeechSequence atomic.Uint64

// menuSpeak hands the menu's requests to speech-dispatcher; tests hear them instead.
var menuSpeak = speechd.Handle

// speakMenu carries out Start Speaking, which reads the focused field's selection, or its whole text without one, stopping whatever speaks, and Stop Speaking, which stops what Start Speaking said.
func (w *window) speakMenu(start bool) {
	if !start {
		webSpeechState.Cancel()
		menuSpeak(menuUtterance, driver.SpeakRequest{Command: driver.SpeakStop}, func(driver.Event) {})
		return
	}
	if web := w.focusedWeb(); web != nil {
		req := webSpeechState.Begin()
		marker := fmt.Sprintf("nsgo-speech-%d", webSpeechSequence.Add(1))
		web.web.speechRequest, web.web.speechMarker = req, marker
		evaluate(web.web.widget, driver.WebEvaluateScript(menuspeech.WebSelection, marker))
		return
	}
	v := w.focusedEntry()
	if v == nil || v.input.Masked && !v.input.Multiline {
		return
	}
	s, e := w.selection(v)
	text := speechText(w.editorText(v), s, e)
	if strings.TrimSpace(text) == "" {
		return
	}
	lang := w.env.Locale
	if lang == "" {
		lang = "en-US"
	}
	menuSpeak(menuUtterance, driver.SpeakRequest{Command: driver.SpeakStart, Lang: lang, Text: text}, func(driver.Event) {})
}

// speechText is what Start Speaking reads of a field's text: the characters from start to end, GTK's offsets, or the whole text when they select none.
func speechText(text string, start, end int) string {
	if start > end {
		start, end = end, start
	}
	if start == end || start < 0 {
		return text
	}
	return text[byteOfChar(text, start):byteOfChar(text, end)]
}

// byteOfChar is the byte offset of character n of text, its length past the end.
func byteOfChar(text string, n int) int {
	for b := range text {
		if n == 0 {
			return b
		}
		n--
	}
	return len(text)
}
