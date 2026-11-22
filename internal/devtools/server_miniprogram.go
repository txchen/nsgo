//go:build nsgodev && js && wasm && nsgominiprogram

package devtools

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall/js"
)

// Mini programs have neither browser fetch nor EventSource: the dev client
// reaches nsgo dev's server through nsgoMP.api's connectSocket (for the
// persistent /_nsgo/events stream and DevTools/automation answers) and
// request (for one-shot HTTP calls such as /_nsgo/reload).

var (
	devWSMu sync.Mutex
	devWS   js.Value
	devWSID uint64
)

func mpAPI() js.Value {
	mp := js.Global().Get("nsgoMP")
	if !mp.Truthy() {
		return js.Undefined()
	}
	return mp.Get("api")
}

func fetch(method, rawURL, contentType string, body io.Reader) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if method == http.MethodGet && u.Path == "/_nsgo/events" {
		return fetchEventsWS(u)
	}
	var data []byte
	if body != nil {
		data, err = io.ReadAll(body)
		if err != nil {
			return nil, err
		}
	}
	if method == http.MethodPost && u.Path == "/_nsgo/devtools/out" && len(data) > 0 {
		if sendDevToolsWS(string(data)) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		}
	}
	return fetchRequest(method, rawURL, contentType, data)
}

func fetchEventsWS(u *url.URL) (*http.Response, error) {
	api := mpAPI()
	if !api.Truthy() || api.Get("connectSocket").Type() != js.TypeFunction {
		return nil, errors.New("mini program API has no connectSocket")
	}
	wsURL := *u
	switch wsURL.Scheme {
	case "https":
		wsURL.Scheme = "wss"
	default:
		wsURL.Scheme = "ws"
	}
	task := api.Call("connectSocket", map[string]any{
		"url":    wsURL.String(),
		"header": map[string]any{"X-NSGo-Token": Token()},
	})
	if !task.Truthy() || task.Get("onMessage").Type() != js.TypeFunction {
		return nil, errors.New("mini program connectSocket returned no socket task")
	}

	isDevTools := u.Query().Has("devtools")
	pr, pw := io.Pipe()
	q := &wsQueue{wake: make(chan struct{}, 1)}
	go q.pump(pw)

	opened := make(chan error, 1)
	var openOnce sync.Once
	reportOpen := func(err error) {
		openOnce.Do(func() { opened <- err })
	}

	var id uint64
	var closeOnce sync.Once
	var onOpen, onMessage, onError, onClose js.Func
	cleanup := func(closeErr error) {
		closeOnce.Do(func() {
			if isDevTools && id != 0 {
				devWSMu.Lock()
				if devWSID == id {
					devWS = js.Undefined()
					devWSID = 0
				}
				devWSMu.Unlock()
			}
			q.close()
			if closeErr != nil {
				reportOpen(closeErr)
			} else {
				reportOpen(errors.New("websocket closed"))
			}
			onOpen.Release()
			onMessage.Release()
			onError.Release()
			onClose.Release()
		})
	}

	onOpen = js.FuncOf(func(_ js.Value, _ []js.Value) any {
		if isDevTools {
			devWSMu.Lock()
			devWSID++
			id = devWSID
			devWS = task
			devWSMu.Unlock()
		}
		reportOpen(nil)
		return nil
	})
	onMessage = js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) == 0 || !args[0].Truthy() {
			return nil
		}
		msg := jsMessageString(args[0].Get("data"))
		q.push("data: " + msg + "\n\n")
		return nil
	})
	onError = js.FuncOf(func(_ js.Value, args []js.Value) any {
		msg := "websocket error"
		if len(args) > 0 && args[0].Truthy() {
			if s := args[0].Get("errMsg"); s.Type() == js.TypeString && s.String() != "" {
				msg = s.String()
			}
		}
		cleanup(errors.New(msg))
		return nil
	})
	onClose = js.FuncOf(func(_ js.Value, _ []js.Value) any {
		cleanup(nil)
		return nil
	})

	task.Call("onOpen", onOpen)
	task.Call("onMessage", onMessage)
	task.Call("onError", onError)
	task.Call("onClose", onClose)

	if err := <-opened; err != nil {
		pr.Close()
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body: &wsReadCloser{
			r: pr,
			close: func() {
				if task.Get("close").Type() == js.TypeFunction {
					task.Call("close", map[string]any{"code": 1000})
				}
				cleanup(nil)
			},
		},
	}, nil
}

func sendDevToolsWS(data string) bool {
	devWSMu.Lock()
	task := devWS
	devWSMu.Unlock()
	if !task.Truthy() || task.Get("send").Type() != js.TypeFunction {
		return false
	}
	done := make(chan bool, 1)
	var once sync.Once
	var success, fail js.Func
	finish := func(ok bool) {
		once.Do(func() {
			success.Release()
			fail.Release()
			done <- ok
		})
	}
	success = js.FuncOf(func(_ js.Value, _ []js.Value) any { finish(true); return nil })
	fail = js.FuncOf(func(_ js.Value, _ []js.Value) any { finish(false); return nil })
	task.Call("send", map[string]any{"data": data, "success": success, "fail": fail})
	return <-done
}

func fetchRequest(method, rawURL, contentType string, data []byte) (*http.Response, error) {
	api := mpAPI()
	if !api.Truthy() || api.Get("request").Type() != js.TypeFunction {
		return nil, errors.New("mini program API has no request")
	}
	header := map[string]any{"X-NSGo-Token": Token()}
	if contentType != "" {
		header["Content-Type"] = contentType
	}
	opts := map[string]any{
		"url":          rawURL,
		"method":       method,
		"header":       header,
		"dataType":     "text",
		"responseType": "text",
	}
	if len(data) > 0 {
		opts["data"] = string(data)
	}
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	var once sync.Once
	var success, fail js.Func
	finish := func(r result) {
		once.Do(func() {
			success.Release()
			fail.Release()
			done <- r
		})
	}
	success = js.FuncOf(func(_ js.Value, args []js.Value) any {
		status := http.StatusOK
		var text string
		if len(args) > 0 && args[0].Truthy() {
			res := args[0]
			if sc := res.Get("statusCode"); sc.Type() == js.TypeNumber {
				status = sc.Int()
			}
			text = jsMessageString(res.Get("data"))
		}
		finish(result{resp: &http.Response{
			StatusCode: status,
			Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(text)),
		}})
		return nil
	})
	fail = js.FuncOf(func(_ js.Value, args []js.Value) any {
		msg := "mini program request failed"
		if len(args) > 0 && args[0].Truthy() {
			if s := args[0].Get("errMsg"); s.Type() == js.TypeString && s.String() != "" {
				msg = s.String()
			}
		}
		finish(result{err: errors.New(msg)})
		return nil
	})
	opts["success"], opts["fail"] = success, fail
	api.Call("request", opts)
	r := <-done
	return r.resp, r.err
}

func jsMessageString(v js.Value) string {
	switch v.Type() {
	case js.TypeString:
		return v.String()
	case js.TypeUndefined, js.TypeNull:
		return ""
	case js.TypeObject:
		if v.InstanceOf(js.Global().Get("ArrayBuffer")) {
			arr := js.Global().Get("Uint8Array").New(v)
			b := make([]byte, arr.Length())
			js.CopyBytesToGo(b, arr)
			return string(b)
		}
		if s := js.Global().Get("JSON").Call("stringify", v); s.Type() == js.TypeString {
			return s.String()
		}
	}
	return v.String()
}

// wsQueue decouples non-blocking JS socket callbacks from the blocking io.PipeWriter
// that bufio.Scanner reads from.
type wsQueue struct {
	mu     sync.Mutex
	items  []string
	closed bool
	wake   chan struct{}
}

func (q *wsQueue) push(msg string) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.items = append(q.items, msg)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *wsQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *wsQueue) pump(pw *io.PipeWriter) {
	defer pw.Close()
	for range q.wake {
		for {
			q.mu.Lock()
			if len(q.items) == 0 {
				closed := q.closed
				q.mu.Unlock()
				if closed {
					return
				}
				break
			}
			msg := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			if _, err := io.WriteString(pw, msg); err != nil {
				return
			}
		}
	}
}

type wsReadCloser struct {
	r     *io.PipeReader
	close func()
}

func (w *wsReadCloser) Read(p []byte) (int, error) { return w.r.Read(p) }

func (w *wsReadCloser) Close() error {
	w.close()
	return w.r.Close()
}
