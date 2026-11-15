//go:build windows

package link

import (
	"context"
	"errors"
	"net/url"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modWinHTTP                    = windows.NewLazySystemDLL("winhttp.dll")
	procWinHTTPDefaultProxyConfig = modWinHTTP.NewProc("WinHttpGetDefaultProxyConfiguration")
	procWinHTTPIEProxyConfig      = modWinHTTP.NewProc("WinHttpGetIEProxyConfigForCurrentUser")
	procWinHTTPOpen               = modWinHTTP.NewProc("WinHttpOpen")
	procWinHTTPGetProxyForURL     = modWinHTTP.NewProc("WinHttpGetProxyForUrl")
	procWinHTTPCloseHandle        = modWinHTTP.NewProc("WinHttpCloseHandle")
	procGlobalFree                = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalFree")
)

const (
	winhttpAccessNoProxy    = 1
	winhttpAccessNamedProxy = 3
	winhttpAutoDetect       = 0x1
	winhttpAutoConfigURL    = 0x2
	winhttpDetectDHCP       = 0x1
	winhttpDetectDNSA       = 0x2
	wpadTimeout             = 15 * time.Second
)

// WINHTTP_PROXY_INFO
type winhttpProxyInfo struct {
	accessType  uint32
	proxy       *uint16
	proxyBypass *uint16
}

// WINHTTP_AUTOPROXY_OPTIONS
type winhttpAutoProxyOptions struct {
	flags                 uint32
	autoDetectFlags       uint32
	autoConfigURL         *uint16
	reserved              uintptr
	reservedDword         uint32
	autoLogonIfChallenged int32
}

// WINHTTP_CURRENT_USER_IE_PROXY_CONFIG
type winhttpIEProxyConfig struct {
	autoDetect    int32
	autoConfigURL *uint16
	proxy         *uint16
	proxyBypass   *uint16
}

// takeString copies a string WinHTTP allocated and frees it (GlobalFree, as its docs say).
func takeString(p *uint16) string {
	if p == nil {
		return ""
	}
	s := windows.UTF16PtrToString(p)
	_, _, _ = procGlobalFree.Call(uintptr(unsafe.Pointer(p)))
	return s
}

func machineProxy(target string) (ProxyChoice, error) {
	u, err := url.Parse(target)
	if err != nil {
		return ProxyChoice{}, err
	}
	// 3. netsh winhttp.
	var pi winhttpProxyInfo
	if r, _, _ := procWinHTTPDefaultProxyConfig.Call(uintptr(unsafe.Pointer(&pi))); r != 0 {
		list, bypass := takeString(pi.proxy), takeString(pi.proxyBypass)
		if pi.accessType == winhttpAccessNamedProxy && list != "" {
			if Bypassed(u.Hostname(), bypass) {
				return ProxyChoice{Source: "netsh winhttp bypass list"}, nil
			}
			if p := ParseWinHTTPProxy(list); p != nil {
				return ProxyChoice{URL: p, Source: "netsh winhttp"}, nil
			}
		}
	}
	// 4. WPAD.
	if list, bypass, err := wpadProxy(target, ""); err == nil && list != "" && !Bypassed(u.Hostname(), bypass) {
		if p := ParseWinHTTPProxy(list); p != nil {
			return ProxyChoice{URL: p, Source: "WPAD"}, nil
		}
	}
	return ProxyChoice{Source: "no machine proxy, no WPAD"}, nil
}

// wpadProxy asks WinHTTP for target's proxy by auto-detection (pac == "") or
// from the PAC file at pac. An empty list means direct.
func wpadProxy(target, pac string) (list, bypass string, err error) {
	type res struct {
		list, bypass string
		err          error
	}
	ch := make(chan res, 1)
	go func() {
		l, b, e := getProxyForURL(target, pac)
		ch <- res{l, b, e}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), wpadTimeout)
	defer cancel()
	select {
	case r := <-ch:
		return r.list, r.bypass, r.err
	case <-ctx.Done():
		return "", "", errors.New("WPAD did not answer in 15 s")
	}
}

func getProxyForURL(target, pac string) (string, string, error) {
	agent, _ := windows.UTF16PtrFromString("digi-erp-connector-link")
	h, _, e := procWinHTTPOpen.Call(uintptr(unsafe.Pointer(agent)), winhttpAccessNoProxy, 0, 0, 0)
	if h == 0 {
		return "", "", e
	}
	defer func() { _, _, _ = procWinHTTPCloseHandle.Call(h) }()
	// No automatic logon: whoever answers for WPAD on the LAN would collect the machine account's NTLM (review
	// 2026-11-15); a proxy that wants a login is a preflight STOP anyway.
	opts := winhttpAutoProxyOptions{autoLogonIfChallenged: 0}
	if pac != "" {
		opts.flags = winhttpAutoConfigURL
		opts.autoConfigURL, _ = windows.UTF16PtrFromString(pac)
	} else {
		opts.flags = winhttpAutoDetect
		opts.autoDetectFlags = winhttpDetectDHCP | winhttpDetectDNSA
	}
	t, _ := windows.UTF16PtrFromString(target)
	var pi winhttpProxyInfo
	r, _, e := procWinHTTPGetProxyForURL.Call(h, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(&opts)), uintptr(unsafe.Pointer(&pi)))
	runtime.KeepAlive(agent)
	runtime.KeepAlive(t)
	runtime.KeepAlive(&opts)
	if r == 0 {
		return "", "", e // ERROR_WINHTTP_AUTODETECTION_FAILED (12180): no WPAD here
	}
	list, bypass := takeString(pi.proxy), takeString(pi.proxyBypass)
	if pi.accessType == winhttpAccessNoProxy {
		return "", "", nil
	}
	return list, bypass, nil
}

// UserProxy is the logged-in user's own proxy setting (IE / Edge, or a PAC file
// by policy), which a LocalSystem service does not see: "" when there is none.
// The preflight names it when the service would go direct.
func UserProxy() string {
	var c winhttpIEProxyConfig
	if r, _, _ := procWinHTTPIEProxyConfig.Call(uintptr(unsafe.Pointer(&c))); r == 0 {
		return ""
	}
	pac, list := takeString(c.autoConfigURL), takeString(c.proxy)
	_ = takeString(c.proxyBypass)
	switch {
	case list != "":
		return "proxy " + list
	case pac != "":
		return "PAC file " + pac
	}
	return ""
}
