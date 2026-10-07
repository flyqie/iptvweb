# IPTV Web Player

一个基于 Go 的 IPTV 播放器，支持 M3U 播放列表管理和 HLS 流代理播放。

## 功能特性

- 📺 Web 界面播放 M3U/M3U8 直播流
- 📡 支持通过独立地址直通 MPEG-TS 直播流
- 📋 左侧频道列表，右侧播放器
- 🔍 支持频道搜索过滤
- 🔄 手动和自动更新 M3U 播放列表
- 🌐 Go 代理解决跨域问题
- 🛡️ 后端生成签名播放地址，前端不接触密钥
- 📱 支持桌面端和移动端抽屉式频道列表

## 快速开始

### 1. 克隆项目

```bash
git clone <your-repo-url>
cd iptvweb
```

### 2. 配置

```bash
# 复制配置文件
cp config.toml.example config.toml

# 编辑配置文件，填入你的 M3U 地址
nano config.toml   # 或使用任意编辑器
```

配置文件示例：

```toml
server_addr = ":8080"

# secret_key for stream anti-hotlinking, empty = disabled
# secret_key = "my-secret-key"

[m3u]
url = "http://example.com/playlist.m3u"
auto_update = 1440
```

`auto_update` 单位是分钟，`0` 或无效值表示关闭自动更新。上面的 `1440` 表示每 24 小时自动更新一次。

### 3. 运行

```bash
# 下载依赖
go mod tidy

# 运行
go run ./cmd/iptvweb
```

### 4. 访问

打开浏览器访问 `http://localhost:8080`

## 配置说明

| 配置项 | 说明 | 默认值 |
|--------|------|--------|
| `server_addr` | 服务器监听地址 | `:8080` |
| `m3u.url` | M3U 播放列表 URL | (必填) |
| `secret_key` | 流媒体防盗链密钥，空值表示关闭 | (空) |
| `m3u.auto_update` | 自动更新间隔（分钟），0 表示关闭 | `0` |
| `m3u.detect_mode` | 频道类型判断方式，`probe` 或 `url` | `probe` |

`secret_key` 配置后，后端会为 `/api/channels` 返回带签名的 `playlist_url`、`stream_url` 和 `type_url`。前端只使用这些地址，不接触密钥或自行拼接签名；只有 playlist、stream、type 和 segment 代理请求需要签名，其他 API 不需要。

`detect_mode` 决定如何判断频道源是 HLS 还是裸 MPEG-TS：

- `probe`（默认）：首次播放该频道时请求上游响应头，按 `Content-Type` 判断并缓存结果。准确，但首次播放多一次请求。
- `url`：只根据 URL 后缀判断（`.m3u8` 视为 HLS，其余视为 TS）。不产生额外请求，但可能误判。

代理会限制上游协议和目标地址，避免 segment 接口被当成任意开放代理使用。源站地址不会出现在频道 API 响应中。

## 前端界面

- 左侧边栏显示频道列表，支持搜索过滤
- 右侧视频播放器：HLS/M3U8 由 hls.js（或浏览器原生 HLS）播放，裸 MPEG-TS 由 mpegts.js 转封装为 MSE 后通过 `<video>` 播放
- 裸 MPEG-TS 播放需要浏览器支持 MediaSource
- hls.js 与 mpegts.js 已内置于二进制，运行时不请求任何外部 CDN
- 顶部"手动更新"按钮即时刷新频道列表
- 显示上次更新时间和频道数量

## API 接口

| 路径 | 方法 | 说明 |
|------|------|------|
| `/` | GET | 播放器页面 |
| `/api/channels` | GET | 获取频道列表 |
| `/api/update` | POST | 手动更新频道列表 |
| `/api/stream/{id}/playlist.m3u8` | GET | 代理获取 M3U8 播放列表 |
| `/api/stream/{id}/stream` | GET | 直通代理频道的 MPEG-TS 流 |
| `/api/stream/{id}/type` | GET | 探测并返回频道源类型（`hls` / `ts`） |
| `/api/stream/{id}/segment?url=...` | GET | 代理获取 TS 分片 |
| `/api/status` | GET | 获取服务器状态 |

## 项目结构

```
iptvweb/
├── cmd/iptvweb/main.go        # 入口文件
├── go.mod / go.sum            # Go 模块文件
├── config.toml.example        # 配置示例
├── internal/
│   ├── config/config.go       # 配置加载
│   ├── m3u/
│   │   ├── parser.go          # M3U 解析
│   │   └── fetcher.go         # M3U 获取、存储和流代理
│   ├── scheduler/scheduler.go # 定时更新
│   └── server/server.go       # HTTP 服务器、代理和安全中间件
├── web/
│   ├── embed.go               # 将前端资源嵌入二进制
│   ├── index.html             # 主页面
│   ├── css/style.css          # 响应式样式
│   └── js/
│       ├── app.js             # 前端状态、搜索和播放逻辑
│       └── vendor/            # hls.js / mpegts.js 本地副本
└── README.md
```

## 构建二进制文件

```bash
# 交叉编译
GOOS=linux GOARCH=amd64 go build -o iptvweb-linux ./cmd/iptvweb
GOOS=windows GOARCH=amd64 go build -o iptvweb.exe ./cmd/iptvweb
GOOS=darwin GOARCH=amd64 go build -o iptvweb-darwin ./cmd/iptvweb
```

## License

MIT
