# boeyu-proxy (POC)

> ⚠️ **POC（概念验证）**：本项目仅用作**技术验证与演示**（proof of concept），
> 用于演示针对博育（boeyu）MDM 接口的请求改写思路，非生产级方案。
> 请勿将其用于未授权环境或真实生产系统；使用时请遵守当地法律法规与相关授权。

面向博育（boeyu）MDM 接口的 **Go 语言 MITM HTTP 代理** POC，用于搭建私有测试服务器（"私服"）。
它把 ProxyPin 的改写规则原样搬进一个独立的 Go 服务里，供测试设备 / 工具通过代理方式接入。

- 唯一入口：4 个端口分组（`huifujiesuo` 9931 / `kaiwang` 9932 / `kangwangjiasuoyou` 9933 / `kaiwangjialanya` 9934），HTTP 代理协议（`CONNECT` 隧道 + absolute-form 请求）；可通过 Web 后台随时**新增/修改端口**，各端口可绑定**不同的功能列表**
- **全量透传**：除命中的改写规则外，所有请求一律解密后原样转发给真实 `s.boeyu.com`（无离线/假数据端口）
- 自签 `*.boeyu.com` 证书，有效期默认 **100 年**
- 仅允许接入 `s.boeyu.com`、`logs.boeyu.com`；`logs.boeyu.com` 请求在**所有端口**本地返回 200 合成 JSON（不透传）；`cdn.boeyu.com` 及其他域名一律拒绝（403）
- **一个开关 = 一个功能**：每条规则都可独立开关、每个端口可绑定不同的功能组合、每个监听器还有总开关；改端口/改功能列表即改即生效并**持久化到配置**（重开保留）

---

## 一、它能做什么

设备 / App 把 HTTP 代理指向本服务后：

- 对 `s.boeyu.com` 之类的 HTTPS 请求，本服务通过 `CONNECT` 建立隧道，用自签 CA 解密流量（MITM），
  命中规则的请求**替换请求/响应**，其余请求一律原样**透传**给真实服务器。
- 功能开组：
  - 每条规则有英文 `id`（程序/API 用）与中文 `name`（后台显示用）：`logout` 登出 / `updateControlState` 管控状态更新覆盖 / `reset` 重置至最新策略 / `logs_clear` 置空日志 / `kaiwang_net2` 网络连接 / `kaiwang_flags` 全功能 / `kaiwang_1004` 蓝牙连接 / `kaiwang_all` 允许全部应用安装 / `all_200` 全体操作请求默许 / `preference_custom` 设备定义参数置空；按端口分组绑定；`updateControlState` 默认**全端口开启**且转发时删除请求体（空 body 发送）；
  - 4 组默认绑定：`huifujiesuo`（复原+置空管理设备密码）=logout+reset+logs_clear+updateControlState；`kaiwang`（打开全局应用网络连接）=kaiwang_net2+logs_clear+updateControlState；`kangwangjiasuoyou`（万能旅馆模式）=kaiwang_all+kaiwang_flags+logs_clear+updateControlState；`kaiwangjialanya`（打开全局应用网络连接和蓝牙）=kaiwang_1004+kaiwang_net2+logs_clear+updateControlState；
所有功能（规则）可通过管理 API 随时上下电，**每个开关互相独立**。

---

## 二、架构与目录

```
proxyboeyu/
├── main.go / config.go / cert.go / rules.go / proxy.go / admin.go   # 全部核心逻辑（纯标准库）；各文件顶部已标注本项目为 POC
├── go.mod                      # Go 1.21+
├── Dockerfile                  # 多阶段构建，golang:1.22-alpine -> alpine:3.20
├── docker-compose.yml          # 直接 `docker compose up -d`
├── config.json              # Docker 部署使用的配置（镜像复制进 /config，名称卷保存）
├── config.local.json        # Windows 本地运行配置
├── rules.json               # 由 proxypin 配置转换生成的规则（生成物）
├── tools/convert.ps1        # 转换脚本（源数据已删除，仅保留占位）
└── data/                       # 运行时目录：自动生成 ca.crt/ca.key、日志
```

一个监听器：

| 监听器名 | 地址 | 模式 | 参数 | 说明 |
|---|---|---|---|---|
`huifujiesuo` | `复原+置空管理设备密码` | `:9931` | 代理模式 `proxy:true` + 透传 `offline:false` | logout / reset / logs_clear / updateControlState | 恢复解锁：登出+重置策略+控制状态
`kaiwang` | `打开全局应用网络连接` | `:9932` | 代理模式 + 透传 | kaiwang_net2 / logs_clear / updateControlState | 开网(net2)：networkMode=2
`kangwangjiasuoyou` | `万能旅馆模式` | `:9933` | 代理模式 + 透传 | kaiwang_all / kaiwang_flags / logs_clear / updateControlState | 开网加所有：白名单+all/* 且 flagList=1001~1099
`kaiwangjialanya` | `打开全局应用网络连接和蓝牙` | `:9934` | 代理模式 + 透传 | kaiwang_1004 / kaiwang_net2 / logs_clear / updateControlState | 开网加蓝丫：flag1004 + networkMode=2

> 规则以英文 `id` 作为程序/API 标识（`/api/listener/{id}/rule/{id}/...`），中文 `name` 仅用于后台展示。`logs` 域名规则（`logs_clear`/置空日志）默认在**所有端口**开启：对 `logs.boeyu.com` 任何请求本地返回 `{"status":200,"msg":"","data":null}`，**不透传**。

> 每个端口就是一个"端口组"，各自绑定不同的功能列表；端口号和功能列表都可在 Web 后台改（改端口后如需对外访问，同步改 compose 端口映射）。

> 代理模式（`proxy: true`）是经典 HTTP 代理协议：
> 客户端设置代理后，对 `https://` URL 发送 `CONNECT host:443`，本服务验证域名是否在白名单，
> 通过后返回 `200 Connection Established`，随后用自签证书做 TLS MITM，再执行改写逻辑。
> `http://` URL 则直接以 absolute-form（`GET http://host/path HTTP/1.1`）处理，同样会命中规则。

---

## 三、Web 管理后台（功能开关）

Docker 部署下浏览器打开 `http://<服务器IP>:9964/` 即可进入可视化后台（本机调试为 `http://127.0.0.1:8080/`），支持：

- 每个功能（规则）一个**开关**，点击即生效、即见状态；
- 每个监听器的**总开关**；
- 每个监听器卡片带 **「编辑端口/功能」**：可改端口、勾选该端口要启用的功能列表，保存即改即生效并写回配置文件（重启保留）；
- 右上角：重载配置、刷新、下载 CA（下载走 token 鉴权）；
- Docker 配置已内置 `admin_token`，首次打开在右上角输入框填入 token 后自动带上（保存在浏览器 localStorage）。**后台对外暴露，务必保留该 token，不要清空。**

> **外部访问管理页的关键点**：
> - Docker/compose 部署：对外用 **9964**（compose 映射 `9964:8080`），需在防火墙/云安全组放行 9964；
> - **直接运行 exe（非 Docker）**：管理页端口就是 `admin_addr`，默认 `http://<IP>:8080`，不是 9964；要外部访问请把配置里 `admin_addr` 写成 `":8080"`（监听所有网卡），并放行该端口；
> - 也可用环境变量 `ADMIN_ADDR=:8080` 覆盖配置里的地址，无需改文件。

后台依赖的 JSON API 同样可直接调用（Docker 下 `http://<服务器IP>:9964/...`，使用 Bearer 鉴权）：

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/healthz` | 存活检查 |
| GET | `/ca.pem` | 下载自签 CA 证书（用于信任） |
| GET | `/api/rules` | 查看各监听器的规则与开关状态 |
| POST | `/api/listener/{name}/enable` | 打开某监听器总开关 |
| POST | `/api/listener/{name}/disable` | 关闭某监听器总开关（请求返回 503） |
| POST | `/api/listener/{name}/rule/{rule}/enable` | 打开某条规则（功能） |
| POST | `/api/listener/{name}/rule/{rule}/disable` | 关闭某条规则（功能） |
| POST | `/api/listener/{name}/port` | 修改端口，body `{"addr":":xxxx"}`；即改即生效并持久化 |
| POST | `/api/listener/{id}/features` | 修改功能列表，body `{"enable_rules":["logout","reset"]}`（规则用英文 id）；即改即生效并持久化 |
| POST | `/api/reload` | 重新读取配置文件与规则文件（新增/删除监听器需重启） |

示例：

```bash
# 临时关闭 public 入口（请求返回 503）
curl -H "Authorization: Bearer TOKEN" -X POST "http://<服务器IP>:9964/api/listener/public/disable"

# 开关某条规则（功能）
curl -H "Authorization: Bearer TOKEN" -X POST "http://<服务器IP>:9964/api/listener/public/rule/reset/enable"
curl -H "Authorization: Bearer TOKEN" -X POST "http://<服务器IP>:9964/api/listener/public/rule/updateControlState/disable"

# 把 p2 端口改到 :56790（立即生效，Docker 下需同步改 compose 端口映射才能从外部访问）
curl -H "Authorization: Bearer TOKEN" -X POST "http://<服务器IP>:9964/api/listener/p2/port" \
  -H "Content-Type: application/json" -d '{"addr":":56790"}'

# 修改 p2 的功能列表
curl -H "Authorization: Bearer TOKEN" -X POST "http://<服务器IP>:9964/api/listener/p2/features" \
  -H "Content-Type: application/json" -d '{"enable_rules":["logout","reset"]}'
```

---

## 四、配置说明

### config.json（Docker 部署）

```jsonc
{
  "data_dir": "/data",            // 运行时数据（自动生成 CA 证书）
  "cert_days": 36500,             // 自签证书有效期（天），36500 ≈ 100 年
  "cert_domains": ["*.boeyu.com","s.boeyu.com","logs.boeyu.com"], // 证书 SAN
  "allowed_hosts": ["s.boeyu.com", "logs.boeyu.com"], // 允许接入的域名（SNI + Host 双重校验）
  "admin_addr": ":8080",            // 管理后台监听地址（Docker 内绑全部网卡：8080 → 对外 9964）
  "admin_token": "xxx",             // 必填；浏览器/curl 通过 Bearer 提交
  "backends": {},                 // 可选：某域名转发到指定后端，如 {"s.boeyu.com":"192.168.1.10:443"}
  "insecure_skip_verify": false,  // 转发时是否跳过对上游证书的校验
  "listeners": [ /* 见下方 */ ]
}
```

每个监听器：

```jsonc
{
  "name": "public",            // 监听器名（管理 API 用）
  "addr": ":9938",            // 监听地址
  "enabled": true,             // 启动时总开关
  "proxy": true,               // true=HTTP 代理端口；false=直连 TLS 服务
"offline": false,            // true=离线假数据（代码保留，当前未使用）；false=全量透传
  "rules_file": "rules.json",  // 规则文件（相对配置目录）
  "enable_rules": ["logout","updateControlState","reset"] // 启动时打开的功能组（英文 id）
}
```

> `enable_rules` 决定启动时**哪些开关打开**。之后随时用管理 API / 后台改。
> 监听器的 `addr` 与 `enable_rules` 改动会**自动写回配置文件**（Web 后台「编辑端口/功能」或上面的 API），
> 因此 Docker 里 `./config` 挂载是**可写**的（不能用 `:ro`）。
> 注意：Docker 改端口后，若需要从外部访问，还必须同步修改 compose 的 `ports` 映射并 `docker compose up -d`。

示例监听器配置（演示"不同端口、不同功能列表"）：

```jsonc
{
  "name": "public",
  "addr": ":9938",
  "enabled": true,
  "proxy": true,
  "offline": false,
  "rules_file": "rules.json",
  "enable_rules": ["logout","updateControlState","reset"]
},
{
  "name": "passthrough",
  "addr": ":9939",
  "enabled": true,
  "proxy": true,
  "offline": false,
  "rules_file": "rules.json",
  "enable_rules": []
}
```

---

## 五、运行方式

### 1) Docker 部署（外部可用）

服务器需能访问真实的 `s.boeyu.com`（透传转发需要）。

```bash
# 镜像已内置 config.json、rules.json（名称卷首次自动填充），无需手动放置
docker compose up -d --build
```

- 9931 / 9932 / 9933 / 9934（4 个端口分组）对外放开，9964（Web 管理后台，需 token）对外放开。
- 配置使用**命名卷**（`boeyu-config`/`boeyu-data`）：首次启动自动从镜像填充默认配置（镜像已内置 `/config`），不会因绑定的空目录导致找不到 `config.json`；Web 后台改端口/功能会写回卷内配置，重启保留。
- 首次启动自动生成 `data/ca.crt`（100 年自签 CA）。

### 2) 本地运行（Windows / macOS / Linux）

需要 [Go 1.21+](https://go.dev/dl/)，或直接使用已编译的 `boeyu-proxy.exe`：

```bash
go build -o boeyu-proxy.exe .
.\boeyu-proxy.exe -config config.local.json
```

启动后：

```
2026/.. ..:..  certificate ready (domains=[*.boeyu.com s.boeyu.com logs.boeyu.com], days=36500) at ./data/ca.crt
2026/.. ..:..  admin web available at http://:8080
2026/.. ..:..  listener "kaiwang" listening on :9932 (proxy=true offline=false master=true)
```

命令行参数：`-config <路径>`，默认 `/config/config.json`（Docker 默认值）。

---

## 六、让测试设备信任证书

因为流量是 MITM 解密/加密的，设备必须信任本服务的 CA：

# 而这就是终极bug！！

> 随机产生测试证书 100 年有效（`cert_days: 36500`）。重复启动不会覆盖已存在的 `ca.crt/ca.key`。

---

## 七、规则数据从哪里来

规则文件为 ProxyPin 兼容格式：`requestReplace`（改写请求行/头/体）与
`responseReplace`（改写响应状态/头/体），body 支持字符串或原始 JSON。

**JSON 部分替换（`replaceJsonBody`）**：类似 `replaceResponseBody`，但只改指定字段、保留其余内容。
关键区别：`replaceJsonBody` 命中时**先透传真实服务器**，拿服务器实时返回的响应体，再按点路径修改后返回设备
（`replaceResponseBody` 仍是本地合成、不透传）。`values.json` 的每个键是响应体 JSON 里的**点路径**
（`status`、`msg`、`data.user`、`data.0.id`…），路径对应的值被覆写；中间节点为 `null` 或缺失时会自动创建对象再下钻。例：

```jsonc
{
  "name": "登出-改token",
  "enabled": true,
  "url": "https://s.boeyu.com/user/user/userLogoutControl",
  "type": "responseReplace",
  "items": [{
    "enabled": true,
    "type": "replaceJsonBody",
    "values": { "json": { "status": 200, "data.token": "xxx" } }
  }]
}
```

响应体不是 JSON 时该条不改写，原样返回（预兼容）。

**追加语义（点路径以 `+` 结尾）**：把值「加进」服务器响应体的现有字段而不是整体覆写——
逗号分隔的字符串字段（如 `deviceInterfaceFlagList`）追加未出现的项（去重）；数组字段（如
`strategySoftwareWhiteList`）追加元素（对象按 `packageName`/`name` 去重）；目标字段缺失时直接创建。例：

```jsonc
{
  "name": "kaiwang_all",
  "enabled": true,
  "url": "https://s.boeyu.com/user/user/getUserStrategy",
  "type": "responseReplace",
  "items": [{
    "enabled": true,
    "type": "replaceJsonBody",
    "values": { "json": { "data.strategySoftwareWhiteList+": [
      { "whiteId": "26031664FYXAFDGR", "logo": "software/25101483F2BPM5AW.png",
        "name": "all", "isWhole": 0, "whiteList": "*", "packageName": "*" }
    ] } }
  }]
}
```
等价地 `"data.strategyDeviceInterface.deviceInterfaceFlagList+": "1004"` 表示确保 flagList 含 1004。

---

## 八、行为速查

| 场景 | 结果 |
|---|---|
| 非白名单域名（`b.com` 等）CONNECT/请求 | 拒绝 |
| `logs.boeyu.com` 走任意端口（log 规则默认全端口开启） | 本地返回 `{"status":200,"msg":"","data":null}`，不透传 |
| 命中 `responseReplace` 规则（登出，全量替换） | **不透传**：本地直接合成替换后的响应返回 |
| 命中 `responseReplace` + `replaceJsonBody` | **透传真实服务器** → 用服务器返回的响应体按点路径实时部分替换（追加用 `path+`），其余保留 |
| 命中 `requestReplace` 规则（reset/updateControlState） | **透传**：请求体改写后转发上游，返回真实响应 |
| 未命中任何规则 | 原样透传（不改动） |
| 监听器总开关关闭 | 503 `function disabled` |
| 上游不可达/出错 | 502 bad gateway |

---

## 九、常见问题

- **代理端口返回 502**：多为真实上游 `s.boeyu.com` 不可达（部署机网络/防火墙），
  检查日志 `-> upstream error: ...`；若是证书校验问题，可临时设 `insecure_skip_verify: true`。
- **设备连不上代理**：确认代理协议用的 `http`（而非 socks），端口对应；域名需在 `allowed_hosts`。
- **改了规则不生效**：POST `/api/reload` 重载；改 `listeners` 结构需重启容器。
- **为什么支持多个端口**：按需给不同设备/场景分配不同端口，每个端口绑定不同的功能列表（如 A 端口带登出、B 端口只透传）。端口和功能列表都可在 Web 后台改，改完自动写回配置，重启保留。
