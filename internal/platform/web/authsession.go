//go:build js && wasm

package web

import (
	"strconv"
	"strings"
	"syscall/js"
	"time"

	"github.com/nsgo-dev/nsgo/internal/driver"
)

// Auth sessions on the web open the provider in a popup window, which keeps the page and the app's state where a redirect of the page itself would lose them. The popup opens blank as the session starts, while the press that started it still lets a page open one, and goes to the authorization URL once the app built it. The provider redirects to a page of the app's origin, whose page sends the callback over a BroadcastChannel so Cross-Origin-Opener-Policy cannot make the opener mistake it for a closed popup; address polling remains the fallback. A popup the user closes ends the session cancelled. The page served at the redirect shows only for that moment.

// webAuth is a popup an auth session shows.
type webAuth struct {
	popup     js.Value
	redirect  string
	channel   js.Value
	channelFn js.Func
	stop      chan struct{}
}

// webAuths are the sessions by number; the page runs them on one thread.
var webAuths = map[uint32]*webAuth{}

// authSession carries out an auth session's request.
func (s *surface) authSession(id uint32, r driver.AuthRequest) {
	dispatch := s.engine.Dispatch
	switch r.Command {
	case driver.AuthStart:
		loc := js.Global().Get("location")
		redirect := loc.Get("origin").String() + r.Path
		switch {
		case r.Scheme == "":
		case strings.EqualFold(r.Scheme, "https") && strings.EqualFold(r.Host, loc.Get("host").String()):
			redirect = "https://" + loc.Get("host").String() + r.Path
		default:
			dispatch(driver.AuthEnd(id, driver.AuthUnavailable, "the web catches only redirects to the page's own origin"))
			return
		}
		popup := js.Global().Call("open", "", "nsgo-auth-"+strconv.FormatUint(uint64(id), 10), "popup,width=520,height=680")
		if !popup.Truthy() {
			dispatch(driver.AuthEnd(id, driver.AuthFailed, "the browser blocked the sign-in popup: start a session from a press"))
			return
		}
		a := &webAuth{popup: popup, redirect: redirect, channel: js.Null(), stop: make(chan struct{})}
		webAuths[id] = a
		if channel := newAuthChannel(redirect); channel.Truthy() {
			a.channel = channel
			a.channelFn = js.FuncOf(func(_ js.Value, args []js.Value) any {
				if len(args) > 0 {
					a.callback(id, args[0].Get("data").String(), dispatch)
				}
				return nil
			})
			channel.Set("onmessage", a.channelFn)
		}
		dispatch(driver.Event{Kind: driver.EventAuthSession, Count: id, Data: &driver.AuthEvent{Phase: driver.AuthRedirect, Redirect: redirect}})
	case driver.AuthOpen:
		a := webAuths[id]
		if a == nil {
			return
		}
		a.popup.Get("location").Set("href", r.URL)
		go a.follow(id, dispatch)
	case driver.AuthCancel:
		if a := webAuths[id]; a != nil {
			a.end(id)
		}
	}
}

// follow reads where the popup is five times a second until it is back at the redirect or closed.
func (a *webAuth) follow(id uint32, dispatch func(driver.Event)) {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-tick.C:
		}
		if a.popup.Get("closed").Bool() {
			// COOP can make an open popup look closed to its opener as soon as
			// the provider redirects it. Give the redirect page's channel
			// message a turn before treating this as a user cancellation.
			select {
			case <-a.stop:
				return
			case <-time.After(500 * time.Millisecond):
			}
			if webAuths[id] != a {
				return
			}
			a.end(id)
			dispatch(driver.AuthEnd(id, driver.AuthCancelled, ""))
			return
		}
		href := popupHref(a.popup)
		if authRedirectMatches(href, a.redirect) {
			a.callback(id, href, dispatch)
			return
		}
	}
}

// callback reports the redirect once. A redirect page can reach this through
// BroadcastChannel even after its provider's COOP severs the opener link.
func (a *webAuth) callback(id uint32, href string, dispatch func(driver.Event)) {
	if webAuths[id] != a || !authRedirectMatches(href, a.redirect) {
		return
	}
	a.end(id)
	dispatch(driver.Event{Kind: driver.EventAuthSession, Count: id, Data: &driver.AuthEvent{Phase: driver.AuthEnded, Callback: href}})
}

func authRedirectMatches(href, redirect string) bool {
	base := strings.SplitN(strings.SplitN(href, "#", 2)[0], "?", 2)[0]
	redirect = strings.SplitN(strings.SplitN(redirect, "#", 2)[0], "?", 2)[0]
	return base != "" && base == redirect
}

func authChannelName(redirect string) string {
	redirect = strings.SplitN(strings.SplitN(redirect, "#", 2)[0], "?", 2)[0]
	return "nsgo-auth:" + redirect
}

func newAuthChannel(redirect string) (channel js.Value) {
	defer func() {
		if recover() != nil {
			channel = js.Null()
		}
	}()
	ctor := js.Global().Get("BroadcastChannel")
	if ctor.Type() != js.TypeFunction {
		return js.Null()
	}
	return ctor.New(authChannelName(redirect))
}

// relayAuthRedirect hands a callback back to the opener from the app page at
// the redirect. It runs on every page load, but sends only a URL with a query
// or fragment, which is how OAuth providers return a result.
func relayAuthRedirect() {
	defer func() { _ = recover() }()
	loc := js.Global().Get("location")
	if loc.Get("search").String() == "" && loc.Get("hash").String() == "" {
		return
	}
	channel := newAuthChannel(loc.Get("origin").String() + loc.Get("pathname").String())
	if !channel.Truthy() {
		return
	}
	channel.Call("postMessage", loc.Get("href").String())
	channel.Call("close")
}

// popupHref is where popup is, "" while it shows another origin's page, whose location the page may not read. Reading it throws then, which only a call turns into a panic Go recovers from, so it reads through Reflect.get.
func popupHref(popup js.Value) (href string) {
	defer func() {
		if recover() != nil {
			href = ""
		}
	}()
	get := js.Global().Get("Reflect").Get("get")
	return get.Invoke(get.Invoke(popup, "location"), "href").String()
}

// end closes the popup and forgets the session.
func (a *webAuth) end(id uint32) {
	if webAuths[id] != a {
		return
	}
	delete(webAuths, id)
	close(a.stop)
	if a.channel.Truthy() {
		a.channel.Set("onmessage", js.Null())
		a.channel.Call("close")
		a.channelFn.Release()
		a.channel = js.Null()
	}
	a.popup.Call("close")
}
