package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
)

// devtoolsPage is the DevTools page nsgo dev -devtools serves: plain HTML and JavaScript, so it opens at once and builds with nothing.
//
//go:embed devtools
var devtoolsPage embed.FS

// The longest line the app may post to /_nsgo/devtools/out, and under -automation, as an answer may hold a screenshot; tests lower them.
var (
	maxMessage = 64 << 20
	maxAnswer  = 256 << 20
)

// automationMessage starts each automation message the app posts.
var automationMessage = []byte(`{"type":"automation"`)

// serveDevTools adds DevTools to the dev server: the page at /_nsgo/devtools/, the app's messages, which it posts to /_nsgo/devtools/out and pages follow at /_nsgo/devtools/stream, and the pages' commands, which /_nsgo/devtools/command passes to the app as devtools events. All but the page's own files take the run's token (handle). The app's automation messages, its answers to nsgo dev -automation's requests, go to the automation alone.
func (s *devServer) serveDevTools() {
	page, _ := fs.Sub(devtoolsPage, "devtools")
	s.mux.Handle("/_nsgo/devtools/", http.StripPrefix("/_nsgo/devtools/", http.FileServer(http.FS(page))))
	s.handle("/_nsgo/devtools/out", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST the app's messages", http.StatusMethodNotAllowed)
			return
		}
		if !fromDevServer(w, r, "application/x-ndjson") {
			return
		}
		s.mu.Lock()
		hub := s.hub
		s.mu.Unlock()
		if status, err := s.deliverDevToolsOut(r.Body, hub != nil); err != nil {
			http.Error(w, err.Error(), status)
		}
	})
	s.handle("/_nsgo/devtools/stream", s.pageStream)
	// The app's directory, for the page's links to TSX files, which source maps name relative to it.
	s.handle("/_nsgo/devtools/project", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"dir": s.dir})
	})
	s.handle("/_nsgo/devtools/command", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST a command", http.StatusMethodNotAllowed)
			return
		}
		if !fromDevServer(w, r, "application/json") {
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var compact bytes.Buffer
		if err == nil {
			err = json.Compact(&compact, body)
		}
		if err != nil {
			http.Error(w, "not a JSON command", http.StatusBadRequest)
			return
		}
		s.send("devtools", compact.String())
	})
}

// deliverDevToolsOut scans NDJSON messages from the app (from POST /_nsgo/devtools/out or an authenticated ?devtools WebSocket) and routes automation answers to the hub and everything else to DevTools pages.
func (s *devServer) deliverDevToolsOut(r io.Reader, fromApp bool) (int, error) {
	s.mu.Lock()
	hub := s.hub
	s.mu.Unlock()
	fromApp = fromApp && hub != nil
	// An answer may carry a screenshot, as base64: under -automation a post takes longer lines.
	limit := maxMessage
	if fromApp {
		limit = maxAnswer
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, min(1<<16, limit)), limit)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		// Only the automation's messages are read here, by the type the app writes first (devMessage); the rest go to the pages as they came.
		if bytes.HasPrefix(line, automationMessage) {
			var a struct {
				Data json.RawMessage `json:"data"`
			}
			if fromApp && json.Unmarshal(line, &a) == nil {
				hub.answered(a.Data)
			}
			continue
		}
		s.toPages(string(line))
	}
	if err := sc.Err(); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, bufio.ErrTooLong) {
			status = http.StatusRequestEntityTooLarge
		}
		// The rest of the post is lost, an answer in it too: the request waiting fails now rather than when it times out.
		if fromApp {
			hub.lost(fmt.Sprintf("the app's answer was lost: %v", err))
		}
		return status, err
	}
	return 0, nil
}

// fromDevServer reports whether r may reach the app through DevTools, answering it when it may not: it must carry contentType, which a web page of another site cannot send without asking first, as the server never allows, and come from no web page or from the server's own. The app posts from Go, the DevTools page and a web app from the server's origin.
func fromDevServer(w http.ResponseWriter, r *http.Request, contentType string) bool {
	if t, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || t != contentType {
		http.Error(w, "send "+contentType, http.StatusUnsupportedMediaType)
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
			http.Error(w, "DevTools take commands from their own page only", http.StatusForbidden)
			return false
		}
	}
	return true
}

// toPages sends every DevTools page msg, dropping it for a page that has not taken the last ones: the page asks again for what it lacks.
func (s *devServer) toPages(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.pages {
		select {
		case ch <- msg:
		default:
		}
	}
}

// pageStream follows the app's messages for a DevTools page.
func (s *devServer) pageStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan string, 256)
	s.mu.Lock()
	s.pages[ch] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pages, ch)
		s.mu.Unlock()
	}()
	flusher, _ := w.(http.Flusher)
	// The page asks the app what it shows once it hears it is there.
	fmt.Fprint(w, "data: {\"type\":\"connected\"}\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			if _, err := fmt.Fprintf(w, "data: %s\n\n", msg); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}
