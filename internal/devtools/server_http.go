//go:build nsgodev && !(js && wasm && nsgominiprogram)

package devtools

import (
	"io"
	"net/http"
)

func fetch(method, url, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-NSGo-Token", Token())
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return http.DefaultClient.Do(req)
}
