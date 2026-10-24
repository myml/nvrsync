# nvrsync — 架构与接口契约

NVR 录像同步 + 在线按天回看。单二进制（Go + 内嵌前端），无外部运行时依赖（可选 ffmpeg 做转码代理）。

## 目录与归属

```
cmd/nvrsync/main.go       入口：flag/env → config → store → syncer → api
internal/config/config.go 配置结构 + 默认值 + 校验
internal/store/store.go   设置 JSON + SQLite 持久化（segments / logs / sync state）
internal/nvr/             NVR 协议客户端（/ds 管理 API + 8000 下载）
internal/syncer/syncer.go 同步引擎（扫描→排队→下载→落盘→记账）
internal/play/play.go     按天 HLS 播放列表 + 可选 ffmpeg 转码缓存
internal/api/api.go       HTTP API + go:embed web
web/                      前端静态资源（内嵌）
```

## 数据模型（设置存 `$DATA/config.json`，录像段/日志/同步状态存 `$DATA/nvrsync.db`）

```go
type Segment struct {
    Ch        int    `json:"ch"`         // 物理通道 1..N
    Start     int64  `json:"start"`      // unix 秒
    End       int64  `json:"end"`
    Size      int64  `json:"size"`       // NVR 侧字节数（media list 给的值）
    EventType int    `json:"event_type"`
    Status    string `json:"status"`     // pending|syncing|done|failed|skipped|local_only
    SyncedAt  int64  `json:"synced_at"`
    Path      string `json:"path"`       // 本地相对路径
    Bytes     int64  `json:"bytes"`      // 实际落盘字节
    Err       string `json:"err"`
}
type LogEntry struct {
    TS    int64  `json:"ts"`
    Level string `json:"level"`  // info|warn|error
    Kind  string `json:"kind"`   // scan|download|play|settings
    Msg   string `json:"msg"`
}
type Settings struct {
    NVRHost     string `json:"nvr_host"`      // 192.168.0.49:8000
    NVRUser     string `json:"nvr_user"`
    NVRPass     string `json:"nvr_pass"`
    AdminPort   int    `json:"admin_port"`    // 管理 API 端口，默认 80（会自动试 8081/8080）
    StorageDir  string `json:"storage_dir"`   // 录像落盘根目录
    Channels    []int  `json:"channels"`      // 要同步的通道
    AutoMinutes int    `json:"auto_minutes"`  // 自动同步间隔（分钟），0=关闭
    RetentionD  int    `json:"retention_days"`// 0=不删
    Transcode   bool   `json:"transcode"`     // 是否用 ffmpeg 生成 H.264 代理
    FFmpeg      string `json:"ffmpeg"`        // ffmpeg 路径
    // 同步截止：latest=最新（now-2min）/ yesterday=昨天 23:59 / days=N 天前 00:00
    Cutoff     string `json:"cutoff"`
    CutoffDays int    `json:"cutoff_days"`
    CatalogDays int   `json:"catalog_days"`   // 后台对账回看天数，0=关闭
    CatalogMinutes int `json:"catalog_minutes"` // 对账间隔（分钟），0=默认 5
    CatalogChannels []int `json:"catalog_channels"` // 对账通道，空=跟随同步通道
    // 登录保护，默认关闭（false 时行为与旧版本完全一致）
    AuthEnabled bool   `json:"auth_enabled"`  // 是否启用登录保护
    AuthUser    string `json:"auth_user"`     // 登录名，空=admin
    AuthHash    string `json:"auth_hash"`     // bcrypt 哈希；只存在于 config.json，Settings() 永远返回空
}
```

锁：store 内部 `sync.RWMutex`，每次写用「临时文件 + rename」保证原子性。

## 关键接口

```go
// store
func Open(dir string) (*Store, error)
func (s *Store) Settings() Settings            // AuthHash 始终为空
func (s *Store) SaveSettings(Settings) error   // 保留已有 AuthHash，不覆盖
func (s *Store) AuthHash() string              // 仅供鉴权使用的真实哈希
func (s *Store) SetPassword(user, hash string) error
func (s *Store) ResetAuth() error              // -reset-auth：清空 auth_enabled/auth_hash
func (s *Store) Segments(f SegFilter) []Segment
func (s *Store) UpsertSegment(Segment) error
func (s *Store) Log(level, kind, msg string)
func (s *Store) Logs(limit int) []LogEntry
func (s *Store) Days(ch int) []DayInfo   // 按天聚合：日期、段数、时长、字节
func (s *Store) SyncState() SyncState

// nvr
func (c *Client) Login() error                       // /ds 登录拿 stok
func (c *Client) MediaList(ch int, from, to int64) ([]MediaSeg, error)
func (c *Client) Download(ch int, from, to int64, w io.Writer, ackInterval time.Duration) (n int64, err error)

// syncer
func New(st *store.Store) *Syncer
func (s *Syncer) Start(req SyncRequest) error   // 异步；返回后进度走 SyncState
func (s *Syncer) Stop()
func (s *Syncer) Running() bool

// play
func (p *Player) DayPlaylist(ch int, date string) ([]byte, error) // HLS m3u8
func (p *Player) EnsureProxy(ch int, date string) error           // 可选 ffmpeg
```

## HTTP API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/health` | `{ok,version,commit,build_date}`（启用登录后仍免登录） |
| GET | `/api/ready` | 就绪检查：SQLite + 存储目录；200/503（免登录） |
| POST | `/api/login` | `{user,password}` → `{ok:true}` + `nvrsync_session` Cookie；同一 IP 连续失败 5 次锁 60 秒 |
| POST | `/api/logout` | 删除当前会话并清除 Cookie |
| POST | `/api/auth/password` | `{user,old_password,new_password}`；未启用时可只设置，启用后必须验证旧密码；成功后重置全部会话 |
| GET/PUT | `/api/settings` | 读/写设置；PUT 后立即生效；`auth_hash` 与 `nvr_pass` 永远不会返回，`nvr_pass_set` 表示是否已保存 NVR 密码；PUT 时 `nvr_pass` 留空表示沿用旧值；启用登录前必须先设置密码 |
| POST | `/api/nvr/test` | 用当前设置登录+列一段，返回 ok/错误 |
| POST | `/api/catalog/refresh` | 后台对账：把 NVR 仍在的录像合并进索引，NVR 已覆盖的标为 `local_only`，从没下过的标 `skipped` |
| POST | `/api/nvr/scan` | `{channels,from,to}` 只列出 NVR 录像写入本地索引（不下载），超过截止时间的段标为 skipped |
| GET | `/api/segments?ch=&from=&to=&status=&limit=&offset=` | 列表 + total |
| POST | `/api/sync/start` | `{channels,from,to,force}`（from/to 可省略=自动） |
| POST | `/api/sync/stop` | |
| GET | `/api/sync/status` | `{running,cur,total,bytes,speed,started_at}` |
| GET | `/api/logs?limit=&level=` | 同步历史日志 |
| GET | `/api/days?ch=` | `[{date,segments,seconds,bytes}]` |
| GET | `/api/playlist?ch=&date=` | m3u8（`Content-Type: application/vnd.apple.mpegurl`） |
| GET | `/media/<ch>/<date>/<file>` | 原始 TS，支持 Range |
| GET | `/` `/sync` `/recordings` `/settings` | SPA 页面（同一 index.html） |
| GET | `/login` | 独立登录页（`web/assets/login.html`，免登录） |

启用登录保护（`auth_enabled=true`）后：

- 免登录：`GET /login`、`/assets/*`（静态资源）、`POST /api/login`、`GET /api/health`；
- 其他 `/api/*` 未登录返回 `401 {"ok":false,"error":"unauthorized"}`；
- 页面路径（`/`、`/sync`、`/recordings`、`/settings`、`/media/*` 等）未登录 302 到 `/login`；
- 会话保存在内存（随进程重启失效），Cookie `nvrsync_session` 为 HttpOnly、SameSite=Lax、Path=/、MaxAge=24h；
- 登录失败日志 `kind=auth`，不记录密码；同一 IP 连续失败 5 次后锁定 60 秒。

忘记密码：启动时加 `-reset-auth`，会清空 `auth_enabled`/`auth_hash` 写回 `config.json` 后正常启动。

## 存储布局

```
$DATA/
  nvrsync.db                      # SQLite：录像段/日志/同步状态
  rec/cam1/2026-10-21/07-00-00_08-00-00.ts
  rec/cam1/2026-10-21/07-00-00_08-00-00.ts.json   # sidecar
  cache/hls/cam1/2026-10-21/...                   # 转码缓存（可选）
```

## UI（3 页，深色、卡片式）

- **同步**：顶部统计卡（NVR 段数 / 已同步 / 待同步 / 失败）；筛选（通道、日期、状态）；
  表格列：通道、时间段、大小、状态、同步时间、操作（立即同步）；底部「同步日志」时间线（level 着色、可折叠、自动刷新）。
- **录像**：天卡片墙（日期、通道数、总时长、大小）→ 点开某天进入时间轴播放器；
  时间轴为一天 24h 的进度条，按段着色，点击跳转；播放器用 hls.js，加载 `/api/playlist`。
- **设置**：分四张卡片——「NVR 连接」「同步与存储」（含同步通道、自动同步间隔、保留、同步截止、转码）「索引（对账）」（对账回看天数、对账间隔、对账通道）「账户安全」（启用登录保护、用户名、设置/修改密码）；保存/测试连接按钮。
