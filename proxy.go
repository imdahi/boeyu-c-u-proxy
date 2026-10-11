// boeyu-proxy (POC)：代理核心（CONNECT 隧道 + 规则改写 + 透传）。
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// runtime is the live, mutable state of one listener.
type runtime struct {
	mu         sync.RWMutex
	cfg        ListenerConfig
	offline    bool
	masterOn   bool
	rulesFile  string
	enableOnly []string
	rules      []Rule
	lnMu       sync.Mutex
	ln         net.Listener
}

func newRuntime(cfg ListenerConfig) *runtime {
	rt := &runtime{
		cfg:        cfg,
		offline:    cfg.Offline,
		masterOn:   cfg.Enabled,
		rulesFile:  cfg.RulesFile,
		enableOnly: cfg.EnableRules,
		rules:      cfg.Rules,
	}
	rt.applyEnableOnlyLocked()
	return rt
}

func (rt *runtime) applyEnableOnlyLocked() {
	if rt.enableOnly == nil {
		return
	}
	set := map[string]bool{}
	for _, n := range rt.enableOnly {
		set[n] = true
	}
	for i := range rt.rules {
		rt.rules[i].Enabled = set[ruleID(rt.rules[i])]
	}
}

func (rt *runtime) reload(cfg ListenerConfig) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.cfg = cfg
	rt.offline = cfg.Offline
	rt.masterOn = cfg.Enabled
	rt.rulesFile = cfg.RulesFile
	rt.enableOnly = cfg.EnableRules
	rt.rules = cfg.Rules
	rt.applyEnableOnlyLocked()
}

func (rt *runtime) setMaster(on bool) {
	rt.mu.Lock()
	rt.masterOn = on
	rt.mu.Unlock()
}

func (rt *runtime) master() bool {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.masterOn
}

func (rt *runtime) offlineStatus() bool {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.offline
}

func (rt *runtime) setRule(id string, on bool) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for i := range rt.rules {
		if ruleID(rt.rules[i]) == id {
			rt.rules[i].Enabled = on
			return true
		}
	}
	return false
}

// setFeatures installs a new enabled-rule list (功能列表) at runtime.
func (rt *runtime) setFeatures(names []string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	for i := range rt.rules {
		rt.rules[i].Enabled = set[ruleID(rt.rules[i])]
	}
	rt.enableOnly = append([]string{}, names...)
}

func (rt *runtime) setListener(ln net.Listener) {
	rt.lnMu.Lock()
	rt.ln = ln
	rt.lnMu.Unlock()
}

func (rt *runtime) closeListener() error {
	rt.lnMu.Lock()
	defer rt.lnMu.Unlock()
	if rt.ln != nil {
		err := rt.ln.Close()
		rt.ln = nil
		return err
	}
	return nil
}

// snapshotCfg returns the current listener config for persistence. Rule bodies
// are not included; they are re-loaded from RulesFile on restart.
func (rt *runtime) snapshotCfg() ListenerConfig {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	c := rt.cfg
	if rt.enableOnly != nil {
		c.EnableRules = append([]string{}, rt.enableOnly...)
	} else {
		c.EnableRules = nil
	}
	c.Rules = nil
	return c
}

// rebind switches a listener to a new address (port) at runtime without a
// restart. On failure the previous address is restored (best effort).
func (s *Server) rebind(name, addr string) error {
	rt := s.runtimes[name]
	if rt == nil {
		return fmt.Errorf("listener %q not found", name)
	}
	if _, err := net.ResolveTCPAddr("tcp", addr); err != nil {
		return fmt.Errorf("invalid addr %q: %w", addr, err)
	}
	rt.mu.Lock()
	oldAddr := rt.cfg.Addr
	rt.cfg.Addr = addr
	rt.mu.Unlock()

	if err := rt.closeListener(); err != nil {
		log.Printf("listener %q close old bind: %v", name, err)
	}
	ln, err := s.bindListener(rt)
	if err != nil {
		rt.mu.Lock()
		rt.cfg.Addr = oldAddr
		rt.mu.Unlock()
		return fmt.Errorf("bind %s: %w", addr, err)
	}
	go s.acceptLoop(rt, ln)
	return nil
}

func (s *Server) setListenFeatures(name string, names []string) error {
	rt := s.runtimes[name]
	if rt == nil {
		return fmt.Errorf("listener %q not found", name)
	}
	rt.setFeatures(names)
	return nil
}

// matched returns the enabled matching rules for a request.
func (rt *runtime) matched(reqHost, reqPath string) []*Rule {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if !rt.masterOn {
		return nil
	}
	var out []*Rule
	for i := range rt.rules {
		r := &rt.rules[i]
		if !r.Enabled {
			continue
		}
		if ruleMatches(r.URL, reqHost, reqPath) {
			out = append(out, r)
		}
	}
	return out
}

type ruleState struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	URL      string `json:"url"`
	Enabled  bool   `json:"enabled"`
	Fallback bool   `json:"fallback"`
}

type listenerState struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Addr     string      `json:"addr"`
	Offline  bool        `json:"offline"`
	MasterOn bool        `json:"master_on"`
	Rules    []ruleState `json:"rules"`
}

func (rt *runtime) state() listenerState {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	st := listenerState{ID: listenerID(rt.cfg), Name: rt.cfg.Name, Addr: rt.cfg.Addr, Offline: rt.offline, MasterOn: rt.masterOn}
	for _, r := range rt.rules {
		st.Rules = append(st.Rules, ruleState{ID: ruleID(r), Name: r.Name, Type: r.Type, URL: r.URL, Enabled: r.Enabled, Fallback: r.Fallback})
	}
	return st
}

// Server ties listeners, rules and the upstream transport together.
type Server struct {
	mu            sync.RWMutex
	settings      *Settings
	cert          *CertStore
	allowed       []string
	runtimes      map[string]*runtime
	configPath    string
	transport     *http.Transport
	httpTransport *http.Transport
}

func NewServer(settings *Settings, cert *CertStore, configPath string) *Server {
	s := &Server{
		settings:      settings,
		cert:          cert,
		allowed:       settings.AllowedHosts,
		runtimes:      map[string]*runtime{},
		configPath:    configPath,
		transport:     newTransport(settings),
		httpTransport: newHTTPTransport(settings),
	}
	for _, lc := range settings.Listeners {
		s.runtimes[listenerID(lc)] = newRuntime(lc)
	}
	return s
}

func newTransport(s *Settings) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host := addr
			if h, _, err := net.SplitHostPort(addr); err == nil {
				host = h
			}
			target := addr
			if te, ok := s.Backends[host]; ok && te != "" {
				target = te
			}
			d := &net.Dialer{Timeout: 15 * time.Second}
			return tls.DialWithDialer(d, network, target, &tls.Config{
				ServerName:         host,
				InsecureSkipVerify: s.InsecureSkipVerify,
			})
		},
	}
}

func newHTTPTransport(s *Settings) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host := addr
			if h, _, err := net.SplitHostPort(addr); err == nil {
				host = h
			}
			target := addr
			if te, ok := s.Backends[host]; ok && te != "" {
				target = te
			}
			d := &net.Dialer{Timeout: 15 * time.Second}
			return d.DialContext(ctx, network, target)
		},
	}
}

func (s *Server) Start() error {
	for _, rt := range s.runtimes {
		go func(rt *runtime) {
			if err := s.serveListener(rt); err != nil {
				log.Printf("listener %q failed: %v", rt.cfg.Name, err)
			}
		}(rt)
	}
	return s.StartAdmin()
}

func (s *Server) allowedList() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.allowed
}

func (s *Server) bindListener(rt *runtime) (net.Listener, error) {
	var ln net.Listener
	var err error
	if rt.cfg.Proxy {
		ln, err = net.Listen("tcp", rt.cfg.Addr)
	} else {
		tlsConf := &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{s.cert.Cert},
			GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				if hello.ServerName != "" && !hostAllowed(hello.ServerName, s.allowedList()) {
					return nil, fmt.Errorf("sni %q is not allowed", hello.ServerName)
				}
				return nil, nil
			},
		}
		ln, err = tls.Listen("tcp", rt.cfg.Addr, tlsConf)
	}
	if err != nil {
		return nil, err
	}
	rt.setListener(ln)
	return ln, nil
}

func (s *Server) acceptLoop(rt *runtime, ln net.Listener) {
	log.Printf("listener %q listening on %s (proxy=%v offline=%v master=%v)", rt.cfg.Name, rt.cfg.Addr, rt.cfg.Proxy, rt.offlineStatus(), rt.master())
	for {
		conn, err := ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		go s.handleConn(rt, conn)
	}
}

func (s *Server) serveListener(rt *runtime) error {
	ln, err := s.bindListener(rt)
	if err != nil {
		return err
	}
	s.acceptLoop(rt, ln)
	return nil
}

// hostAllowed checks a hostname against an allowlist of exact names and
// "*.boeyu.com" style patterns.
func hostAllowed(host string, allowed []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if a == host {
			return true
		}
		if strings.HasPrefix(a, "*.") {
			suffix := a[1:] // ".boeyu.com"
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
		}
	}
	return false
}

// requestHost extracts the effective hostname from a request.
func requestHost(req *http.Request) string {
	src := req.Host
	if req.URL != nil && req.URL.IsAbs() {
		src = req.URL.Host
	}
	host := src
	if strings.HasPrefix(host, "//") {
		host = host[2:]
	} else if strings.Contains(host, "://") {
		if u, err := url.Parse(host); err == nil {
			host = u.Host
		}
	}
	if p := strings.LastIndexByte(host, ':'); p >= 0 && !strings.Contains(host[:p], "]") {
		host = host[:p]
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

func (s *Server) handleConn(rt *runtime, conn net.Conn) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	br := bufio.NewReader(conn)
	idle := time.Duration(s.settings.IdleTimeoutSec) * time.Second
	for {
		conn.SetReadDeadline(time.Now().Add(idle))
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if strings.EqualFold(req.Header.Get("Expect"), "100-continue") {
			if _, werr := conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); werr != nil {
				return
			}
			req.Header.Del("Expect")
		}
		keepAlive := !req.Close && !strings.EqualFold(req.Header.Get("Connection"), "close")

		if rt.cfg.Proxy && req.Method == http.MethodConnect {
			s.handleConnect(rt, conn, br, req, remote)
			return
		}
		if rt.cfg.Proxy && !req.URL.IsAbs() {
			log.Printf("[%s] %s %s %s -> 400 proxy requires absolute-form/CONNECT", rt.cfg.Name, remote, req.Method, req.RequestURI)
			writeSimple(conn, 400, "proxy: absolute-form request required", keepAlive)
			return
		}

		reqHost := requestHost(req)
		if !s.handleOne(rt, conn, req, remote, reqHost, keepAlive) {
			return
		}
	}
}

// prependConn feeds any bytes already buffered by the parser before falling
// back to the raw connection (used so a pipelined TLS ClientHello after
// CONNECT is not lost).
type prependConn struct {
	net.Conn
	r *bufio.Reader
}

func (c prependConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// handleConnect MITMs a CONNECT tunnel from a proxy client: respond 200, wrap
// the connection in TLS with our certificate for the requested host, then
// serve the inner HTTPS requests through the normal pipeline. Hosts outside
// the allowlist are refused outright — unrelated traffic is fully blocked.
func (s *Server) handleConnect(rt *runtime, conn net.Conn, br *bufio.Reader, req *http.Request, remote string) {
	reqHost := requestHost(req)
	if !hostAllowed(reqHost, s.allowedList()) {
		log.Printf("[%s] %s CONNECT %s -> silently dropped (not allowed)", rt.cfg.Name, remote, reqHost)
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	tlsConf := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{s.cert.Cert},
	}
	ts := tls.Server(prependConn{conn, br}, tlsConf)
	ts.SetDeadline(time.Now().Add(15 * time.Second))
	if err := ts.Handshake(); err != nil {
		log.Printf("[%s] %s CONNECT %s -> tls handshake failed: %v", rt.cfg.Name, remote, reqHost, err)
		return
	}
	log.Printf("[%s] %s CONNECT %s -> tunnel established", rt.cfg.Name, remote, reqHost)
	ts.SetDeadline(time.Time{})
	s.loop(rt, ts, remote)
}

// loop serves a sequence of HTTP requests on one connection, applying the
// normal rule pipeline (offline synthetic or upstream forward).
func (s *Server) loop(rt *runtime, conn net.Conn, remote string) {
	idle := time.Duration(s.settings.IdleTimeoutSec) * time.Second
	br := bufio.NewReader(conn)
	for {
		conn.SetReadDeadline(time.Now().Add(idle))
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if strings.EqualFold(req.Header.Get("Expect"), "100-continue") {
			if _, werr := conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); werr != nil {
				return
			}
			req.Header.Del("Expect")
		}
		keepAlive := !req.Close && !strings.EqualFold(req.Header.Get("Connection"), "close")
		if req.Method == http.MethodConnect {
			log.Printf("[%s] %s nested CONNECT rejected", rt.cfg.Name, remote)
			writeSimple(conn, 400, "nested CONNECT not supported", keepAlive)
			return
		}
		reqHost := requestHost(req)
		if !s.handleOne(rt, conn, req, remote, reqHost, keepAlive) {
			return
		}
	}
}

func (s *Server) handleOne(rt *runtime, conn net.Conn, req *http.Request, remote, reqHost string, keepAlive bool) bool {
	if !hostAllowed(reqHost, s.allowedList()) {
		log.Printf("[%s] %s %s %s -> silently dropped (not allowed)", rt.cfg.Name, remote, req.Method, reqHost)
		drainBody(req)
		return false
	}

	if req.URL == nil {
		req.URL = &url.URL{}
	}
	if !req.URL.IsAbs() {
		req.URL.Scheme = "https"
		req.URL.Host = reqHost
		req.Host = reqHost
	} else {
		req.Host = reqHost
	}
	req.RequestURI = ""

	reqPath := req.URL.Path
	matched := rt.matched(reqHost, reqPath)
	// Fallback rules (e.g. a catch-all "everything 200") only apply when no
	// specific rule matched, so they can never interfere with the normal rules.
	hasPrimary := false
	for _, m := range matched {
		if !m.Fallback {
			hasPrimary = true
			break
		}
	}
	if hasPrimary {
		var eff []*Rule
		for _, m := range matched {
			if !m.Fallback {
				eff = append(eff, m)
			}
		}
		matched = eff
	}

	// Buffer the request body only when an enabled requestReplace rule will
	// read it (replaceRequestBody / replaceJsonBody). For everything else we
	// let the upstream transport stream the body straight through, avoiding a
	// full in-memory copy and the latency of waiting for the whole body.
	needReqBuffer := false
	for _, m := range matched {
		if m.Type == "requestReplace" {
			needReqBuffer = true
			break
		}
	}
	if needReqBuffer && req.Body != nil {
		body, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			writeSimple(conn, 400, "cannot read request body", keepAlive)
			return keepAlive
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	req.Header.Del("Transfer-Encoding")

	reqHits := applyRequestReplace(req, matched)

	// 命中 responseReplace（如登出，全量替换）则本地直接合成响应、不透传；
	// 命中 responseReplace 且含 replaceJsonBody（字段级部分替换）时，需要
	// 先透传到真实服务器、用服务器返回的响应体按点路径实时打补丁。
	matchesResponse := false
	needsUpstream := false
	for _, m := range matched {
		if m.Type != "responseReplace" {
			continue
		}
		matchesResponse = true
		for i := range m.Items {
			it := &m.Items[i]
			if it.Enabled && strings.EqualFold(it.Type, "replaceJsonBody") {
				needsUpstream = true
			}
		}
	}

	if rt.offlineStatus() || matchesResponse {
		if !rt.master() {
			writeSimple(conn, 503, "function disabled", keepAlive)
			return keepAlive
		}
		if !rt.offlineStatus() && needsUpstream {
			resp, err := s.forward(req, true)
			if err != nil {
				log.Printf("[%s] %s %s %s%s -> upstream error: %v", rt.cfg.Name, remote, req.Method, reqHost, req.URL.Path, err)
				drainBody(req)
				writeSimple(conn, 502, "bad gateway", keepAlive)
				return keepAlive
			}
			modified := applyResponseReplace(resp, matched)
			finalizeResponse(resp, modified)
			writeResponse(conn, resp, keepAlive)
			drainBody(req)
			resp.Body.Close()
			logReq(rt.cfg.Name, remote, req, reqHost, resp, matched, reqHits)
			return keepAlive
		}
		drainBody(req)
		resp := buildSynthetic(req, matched, keepAlive)
		writeResponse(conn, resp, keepAlive)
		logReq(rt.cfg.Name, remote, req, reqHost, resp, matched, reqHits)
		return keepAlive
	}

	resp, err := s.forward(req, false)
	if err != nil {
		log.Printf("[%s] %s %s %s%s -> upstream error: %v", rt.cfg.Name, remote, req.Method, reqHost, req.URL.Path, err)
		drainBody(req)
		writeSimple(conn, 502, "bad gateway", keepAlive)
		return keepAlive
	}
	// Pure passthrough (no response rule matched): stream the upstream body
	// to the client without buffering it all in memory first — lower latency
	// and constant memory, regardless of payload size.
	drainBody(req)
	applyResponseReplace(resp, matched)
	finalizeResponse(resp, false)
	writeResponse(conn, resp, keepAlive)
	resp.Body.Close()
	logReq(rt.cfg.Name, remote, req, reqHost, resp, matched, reqHits)
	return keepAlive
}

func (s *Server) forward(req *http.Request, bufferBody bool) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.RequestURI = ""
	out.Host = req.Host
	var resp *http.Response
	var err error
	if strings.EqualFold(out.URL.Scheme, "http") {
		resp, err = s.httpTransport.RoundTrip(out)
	} else {
		out.URL.Scheme = "https"
		resp, err = s.transport.RoundTrip(out)
	}
	if err != nil {
		return nil, err
	}
	if !bufferBody {
		// Streaming passthrough: hand the upstream body straight to the
		// client writer, never buffering the whole payload in memory.
		return resp, nil
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return resp, nil
}

// drainBody consumes any request-body bytes that have not been sent upstream
// yet, so the next request on this keep-alive connection is not corrupted by
// leftover bytes (important whenever we respond before reading the body).
func drainBody(req *http.Request) {
	if req.Body != nil {
		io.Copy(io.Discard, req.Body)
	}
}

// buildSynthetic serves a canned response in offline mode without contacting
// any upstream server.
func buildSynthetic(req *http.Request, matched []*Rule, keepAlive bool) *http.Response {
	defaultBody := []byte(`{"status":200,"msg":null,"data":null}`)
	resp := &http.Response{
		StatusCode:    200,
		Status:        "200 OK",
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{},
		Body:          io.NopCloser(bytes.NewReader(defaultBody)),
		ContentLength: int64(len(defaultBody)),
		Request:       req,
		Close:         !keepAlive,
	}
	modified := applyResponseReplace(resp, matched)
	if !modified && len(matched) == 0 {
		resp.StatusCode = 404
		resp.Status = "404 Not Found"
		setResponseBody(resp, []byte(`{"status":404,"msg":"offline: no rule matched this request","data":null}`))
	}
	if resp.Header.Get("Content-Type") == "" {
		resp.Header.Set("Content-Type", "application/json")
	}
	finalizeResponse(resp, true)
	return resp
}

// writeSimple writes a plain-text error response.
func writeSimple(conn net.Conn, status int, msg string, keepAlive bool) {
	body := []byte(msg + "\n")
	resp := &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Close:         !keepAlive,
	}
	resp.Header.Set("Content-Type", "text/plain; charset=utf-8")
	writeResponse(conn, resp, keepAlive)
}

func writeResponse(conn net.Conn, resp *http.Response, keepAlive bool) {
	conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	resp.Close = !keepAlive
	resp.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	if resp.ContentLength >= 0 && resp.Header.Get("Content-Length") == "" {
		resp.Header.Set("Content-Length", strconv.Itoa(int(resp.ContentLength)))
	}
	if err := resp.Write(conn); err != nil {
		log.Printf("write response: %v", err)
	}
}

func logReq(name, remote string, req *http.Request, reqHost string, resp *http.Response, matched []*Rule, hits int) {
	names := make([]string, 0, len(matched))
	for _, m := range matched {
		names = append(names, m.Name)
	}
	scheme := req.URL.Scheme
	if scheme == "" {
		scheme = "https"
	}
	log.Printf("[%s] %s %d %s %s://%s%s rules=[%s] hits=%d",
		name, remote, resp.StatusCode, req.Method, scheme, reqHost, req.URL.Path, strings.Join(names, ","), hits)
}
