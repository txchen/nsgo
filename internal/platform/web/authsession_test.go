//go:build js && wasm

package web

import "testing"

func TestAuthRedirectChannelAndMatch(t *testing.T) {
	for _, test := range []struct {
		href, redirect string
		match          bool
	}{
		{"https://app.example/oauth/callback?code=ok&state=s", "https://app.example/oauth/callback", true},
		{"https://app.example/oauth/callback#done", "https://app.example/oauth/callback", true},
		{"https://app.example/other?code=ok", "https://app.example/oauth/callback", false},
		{"", "https://app.example/oauth/callback", false},
	} {
		if got := authRedirectMatches(test.href, test.redirect); got != test.match {
			t.Errorf("authRedirectMatches(%q, %q) = %v, want %v", test.href, test.redirect, got, test.match)
		}
	}
	if got, want := authChannelName("https://app.example/oauth/callback?code=ignored"), "nsgo-auth:https://app.example/oauth/callback"; got != want {
		t.Errorf("authChannelName = %q, want %q", got, want)
	}
}
