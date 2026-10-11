// boeyu-proxy (POC)：管理后台 Web 界面与开关 API。
package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// StartAdmin starts the plain-HTTP administrative API (bound to AdminAddr).
// When no host part is given (e.g. ":8080") it listens on every interface, so
// the page + API are reachable from outside; ADMIN_ADDR env can override the
// config value without editing files.
func (s *Server) StartAdmin() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/ca.pem", s.handleCA)
	mux.HandleFunc("/api/rules", s.handleRules)
	mux.HandleFunc("/api/reload", s.handleReload)
	mux.HandleFunc("/api/listener/", s.handleListenerAction)

	addr := s.settings.AdminAddr
	if env := os.Getenv("ADMIN_ADDR"); env != "" {
		addr = env
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("admin listen %s: %w", addr, err)
	}

	var h http.Handler = mux
	if s.settings.AdminToken != "" {
		h = s.adminAuth(mux)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("admin server: %v", err)
		}
	}()
	display := addr
	if i := strings.IndexByte(addr, ':'); i == 0 {
		display = "0.0.0.0" + addr
	}
	log.Printf("admin web available at http://%s%s", display, noteAdminExt(addr))
	return nil
}

func noteAdminExt(addr string) string {
	if strings.HasPrefix(addr, "127.0.0.1:") || strings.HasPrefix(addr, "localhost:") {
		return " (仅本机可访问；要外部访问请改为 \":端口\" 或设置 ADMIN_ADDR)"
	}
	return " (Docker 对外端口通常映射：compose 里 \"9996:8080\")"
}

func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.settings.AdminToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(adminPage))
}

const adminPage = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Boeyu Proxy 管理后台 (POC)</title>
<link rel="icon" href="data:,">
<style>
  :root { color-scheme: light dark; }
  body { font-family: system-ui, -apple-system, "Segoe UI", "Microsoft YaHei", sans-serif; margin: 0; background: #f5f6f8; color: #1c1e21; }
  .wrap { max-width: 860px; margin: 24px auto; padding: 0 16px; }
  header { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
  h1 { font-size: 20px; margin: 0; }
  .card { background: #fff; border: 1px solid #e2e4e8; border-radius: 12px; padding: 14px 20px; margin-top: 16px; box-shadow: 0 1px 2px rgba(0,0,0,.04); }
  .row { display: flex; align-items: center; gap: 12px; padding: 10px 0; border-bottom: 1px solid #eef0f3; }
  .row:last-child { border-bottom: none; }
  .meta { flex: 1; min-width: 0; }
  .nm { font-weight: 600; }
  .url { font-size: 12px; color: #84868a; word-break: break-all; }
  .status { font-size: 12px; color: #5a5d63; margin-top: 2px; }
  .badge { font-size: 11px; padding: 2px 8px; border-radius: 999px; background: #eef0f3; color: #5a5d63; margin-left: 6px; }
  .btn { border: 1px solid #d0d3d7; background: #fff; border-radius: 8px; padding: 6px 12px; cursor: pointer; font-size: 13px; text-decoration: none; display: inline-block; }
  .btn:hover { background: #f0f1f3; }
  .token { padding: 6px 10px; border: 1px solid #d0d3d7; border-radius: 8px; font-size: 13px; width: 200px; }
  .switch { position: relative; width: 44px; height: 24px; flex: none; }
  .switch input { display: none; }
  .slider { position: absolute; inset: 0; background: #cfd3d8; border-radius: 24px; transition: .15s; cursor: pointer; }
  .slider::before { content: ""; position: absolute; width: 18px; height: 18px; left: 3px; top: 3px; border-radius: 50%; background: #fff; transition: .15s; box-shadow: 0 1px 2px rgba(0,0,0,.2); }
  .switch input:checked + .slider { background: #2f7de1; }
  .switch input:checked + .slider::before { transform: translateX(20px); }
  .muted { color: #84868a; }
  #msg { font-size: 13px; min-height: 18px; margin-top: 10px; }
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1>Boeyu Proxy 管理后台 (POC)</h1>
    <div style="display:flex; gap:8px; align-items:center; flex-wrap:wrap;">
      <input class="token" id="token" type="password" placeholder="admin_token（留空则不填）">
      <button class="btn" onclick="reloadCfg()">重载配置</button>
      <button class="btn" onclick="refresh()">刷新</button>
      <a class="btn" onclick="dlCA()" href="javascript:void(0)">下载 CA</a>
    </div>
  </header>
  <div id="msg" class="muted"></div>
  <div id="app"></div>
</div>
<script>
var TOKEN = localStorage.getItem("bp_token") || "";
var inp = document.getElementById("token");
if (TOKEN) inp.value = TOKEN;
inp.addEventListener("input", function(){ TOKEN = inp.value; localStorage.setItem("bp_token", TOKEN); });
inp.addEventListener("keydown", function(e){ if (e.key === "Enter") { inp.blur(); refresh(); } });
inp.addEventListener("change", function(){ TOKEN = inp.value; localStorage.setItem("bp_token", TOKEN); refresh(); });

function dlCA(){
  fetch("/ca.pem", { headers: authHeaders() }).then(function(resp){
    if (resp.status === 401) { msg("未授权：请先填写 admin_token"); return; }
    return resp.blob();
  }).then(function(b){
    if (!b) return;
    var a = document.createElement("a");
    a.href = URL.createObjectURL(b);
    a.download = "ca.crt";
    document.body.appendChild(a); a.click(); a.remove();
    URL.revokeObjectURL(a.href);
  }).catch(function(e){ msg("下载失败：" + e); });
}

function msg(s){ document.getElementById("msg").textContent = s || ""; }
function esc(s){ var d = document.createElement("div"); d.textContent = (s == null) ? "" : String(s); return d.innerHTML; }
function enc(s){ return encodeURIComponent(s); }

function api(path){
  fetch("/api/" + path, { method: "POST", headers: authHeaders() }).then(function(resp){
    if (resp.status === 401) { msg("未授权：请在右上角填写 admin_token"); return; }
    if (!resp.ok) { msg("操作失败 HTTP " + resp.status); return; }
    msg("已生效：" + path);
    refresh();
  }).catch(function(e){ msg("请求失败：" + e); });
}
function apiBody(path, body){
  var h = authHeaders();
  h["Content-Type"] = "application/json";
  fetch("/api/" + path, { method: "POST", headers: h, body: JSON.stringify(body) }).then(function(resp){
    if (resp.status === 401) { msg("未授权：请在右上角填写 admin_token"); return; }
    if (!resp.ok) { return resp.text().then(function(t){ msg("操作失败: " + t); }); }
    msg("已保存：" + path);
    refresh();
  }).catch(function(e){ msg("请求失败：" + e); });
}
function authHeaders(){
  var h = {};
  if (TOKEN) h["Authorization"] = "Bearer " + TOKEN;
  return h;
}
function doMaster(l, checked){ api("listener/" + l + "/" + (checked ? "enable" : "disable")); }
function doRule(l, r, checked){ api("listener/" + l + "/rule/" + r + "/" + (checked ? "enable" : "disable")); }

function makeSwitch(on, l, r){
  var cb = (r == null) ? 'onchange="doMaster(\'' + l + '\', this.checked)"' : 'onchange="doRule(\'' + l + '\', \'' + r + '\', this.checked)"';
  return '<label class="switch"><input type="checkbox" ' + (on ? "checked" : "") + " " + cb + '><span class="slider"></span></label>';
}

function toggleEdit(idx){
  var el = document.getElementById("edit_" + idx);
  if (el) el.style.display = (el.style.display === "none") ? "block" : "none";
}

function saveEdit(idx, name){
  var el = document.getElementById("edit_" + idx);
  var port = document.getElementById("port_" + idx).value;
  var names = [];
  el.querySelectorAll("[data-rule]").forEach(function(cb){ if (cb.checked) names.push(cb.dataset.rule); });
  apiBody("listener/" + name + "/port", { addr: port });
  apiBody("listener/" + name + "/features", { enable_rules: names });
}

function listenerCard(st, idx){
  var h = '<div class="card">';
  h += '<div class="row"><div class="meta"><div class="nm">' + esc(st.name) + '<span class="badge">' + esc(st.id) + '</span><span class="badge">' + esc(st.addr) + '</span><span class="badge">' + (st.offline ? "离线" : "透传") + '</span></div><div class="status">总开关' + (st.master_on ? "开" : "关") + '</div></div>';
  h += makeSwitch(st.master_on, enc(st.id), null);
  h += '<button class="btn" onclick="toggleEdit(' + idx + ')">编辑端口/功能</button>';
  h += '</div>';
  for (var i = 0; i < (st.rules || []).length; i++) {
    var r = st.rules[i];
    h += '<div class="row"><div class="meta"><div class="nm">' + esc(r.name) + '<span class="badge">' + esc(r.id) + '</span><span class="badge">' + (r.type === "requestReplace" ? "改请求" : "改响应") + '</span>' + (r.fallback ? '<span class="badge">兜底</span>' : '') + '</div><div class="url">' + esc(r.url) + '</div></div>';
    h += makeSwitch(r.enabled, enc(st.id), enc(r.id)) + '</div>';
  }
  h += '<div id="edit_' + idx + '" style="display:none;border-top:1px solid #eef0f3;padding:12px 0;">';
  h += '<div style="font-size:13px;font-weight:600;margin-bottom:6px;">端口（如 :9931）</div>';
  h += '<input class="token" id="port_' + idx + '" value="' + esc(st.addr) + '">';
  h += '<div style="font-size:13px;font-weight:600;margin:12px 0 6px;">功能列表（勾选该端口启用的功能，保存后重启保留）</div>';
  for (var k = 0; k < (st.rules || []).length; k++) {
    var r2 = st.rules[k];
    h += '<label style="display:block;margin:4px 0;font-size:13px;"><input type="checkbox" data-rule="' + esc(r2.id) + '"' + (r2.enabled ? " checked" : "") + '> ' + esc(r2.name) + ' <span class="badge">' + esc(r2.id) + '</span><span class="badge">' + (r2.type === "requestReplace" ? "改请求" : "改响应") + '</span>' + (r2.fallback ? '<span class="badge">兜底</span>' : '') + '</label>';
  }
  h += '<div style="margin-top:12px;"><button class="btn" onclick="saveEdit(' + idx + ", '" + enc(st.id) + "')" + '">保存</button> <button class="btn" onclick="toggleEdit(' + idx + ')">取消</button></div>';
  h += '</div></div>';
  return h;
}

function refresh(){
  fetch("/api/rules", { headers: authHeaders() }).then(function(resp){
    if (resp.status === 401) { msg("未授权：请在右上角填写 admin_token"); return; }
    return resp.json();
  }).then(function(list){
    if (!list) return;
    var html = "";
    for (var i = 0; i < list.length; i++) html += listenerCard(list[i], i);
    document.getElementById("app").innerHTML = html;
    msg("");
  }).catch(function(e){ msg("加载失败：" + e); });
}
function reloadCfg(){ api("reload"); }
refresh();
</script>
</body>
</html>`

func (s *Server) handleCA(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Write(s.cert.CAPEM)
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	states := make([]listenerState, 0, len(s.runtimes))
	for _, rt := range s.runtimes {
		states = append(states, rt.state())
	}
	sort.SliceStable(states, func(i, j int) bool {
		return addrPort(states[i].Addr) < addrPort(states[j].Addr)
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(states)
}

// addrPort extracts the numeric port from an addr like ":56789" or
// "0.0.0.0:56789"; non-numeric tails sort as 0.
func addrPort(addr string) int {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		if n, err := strconv.Atoi(addr[i+1:]); err == nil {
			return n
		}
	}
	return 0
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	err := s.reload()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintln(w, `{"ok":true}`)
}

func (s *Server) handleListenerAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// api/listener/{name}/enable|disable
	// api/listener/{name}/port|features
	// api/listener/{name}/rule/{rule}/enable|disable
	if len(parts) == 6 && parts[0] == "api" && parts[1] == "listener" && parts[3] == "rule" {
		rt := s.runtimes[parts[2]]
		ok := false
		if rt != nil {
			switch parts[5] {
			case "enable":
				ok = rt.setRule(parts[4], true)
			case "disable":
				ok = rt.setRule(parts[4], false)
			}
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"ok":true}`)
		return
	}
	if len(parts) != 4 || parts[0] != "api" || parts[1] != "listener" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rt := s.runtimes[parts[2]]
	if rt == nil {
		http.Error(w, "listener not found", http.StatusNotFound)
		return
	}
	action := parts[3]
	switch action {
	case "enable":
		rt.setMaster(true)
	case "disable":
		rt.setMaster(false)
	case "port", "features":
		var in struct {
			Addr        string   `json:"addr"`
			EnableRules []string `json:"enable_rules"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		var err error
		if action == "port" {
			err = s.rebind(parts[2], in.Addr)
		} else {
			err = s.setListenFeatures(parts[2], in.EnableRules)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if perr := s.persist(); perr != nil {
			log.Printf("persist config: %v", perr)
		}
	default:
		http.Error(w, "unknown action", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintln(w, `{"ok":true}`)
}

// reload re-reads the config and refreshes each existing listener's rules and
// flags. Adding/removing listeners still requires a restart.
func (s *Server) reload() error {
	settings, err := LoadSettings(s.configPath)
	if err != nil {
		return err
	}
	cfgs := map[string]ListenerConfig{}
	for _, lc := range settings.Listeners {
		cfgs[listenerID(lc)] = lc
	}
	added, removed := []string{}, []string{}
	for name := range cfgs {
		if _, ok := s.runtimes[name]; !ok {
			added = append(added, name)
		}
	}
	for name := range s.runtimes {
		if _, ok := cfgs[name]; !ok {
			removed = append(removed, name)
		}
	}
	for name, lc := range cfgs {
		if rt, ok := s.runtimes[name]; ok {
			rt.reload(lc)
		}
	}
	s.mu.Lock()
	s.settings = settings
	s.allowed = settings.AllowedHosts
	s.mu.Unlock()
	if len(added) > 0 || len(removed) > 0 {
		return fmt.Errorf("listener set changed (added=%v removed=%v): restart needed, rules updated", added, removed)
	}
	return nil
}
