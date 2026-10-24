# nvrsync 优化清单（10 类 / 60+ 点）

> 体检时间：2026-10-25
> 线上入口：`https://nvrsync.keywaishe.com/`
> 实际后端：`http://192.168.0.22:8099`（fnOS + rootless podman，容器 `nvrsync`）
> LXC100 的角色：traefik 路由 `nvrsync.keywaishe.com` → `192.168.0.22:8099`，**只做反向代理**
> 线上镜像：`ghcr.io/myml/nvrsync:main`，revision `4d2f381`；仓库 HEAD 已是 `2c9c57a`

## 本轮已落地（已通过 CI 并部署验证）

已修改代码并补测试，**功能提交 `debdb2d`** 已构建为镜像并部署到线上（`/api/health` 返回该 SHA），覆盖：**1.1**（本次部署已启用登录保护，密码由部署时生成并保存在 NAS，不写入仓库）、**1.2**（设置脱敏 + 空值保留）、**1.3**（代理后客户端 IP）、**1.4**（Cookie Secure）、**1.5**（安全头）、**1.6**（Origin/Content-Type 防 CSRF）、**2.2**（脚本移除硬编码密码）、**2.4**（通道去重/配置校验）、**3.1/3.2**（-40401 重试 + 保留 pending）、**3.3/3.6**（忙时熔断 + 自动同步退避）、**4.1/4.2**（retention 覆盖 local_only 且对账后执行）、**4.5**（catalog 只计真正新增，skipped 可恢复）、**5.1/5.2**（迁移 logs.ch、写入 ch）、**6.1**（gzip）、**6.5**（轮询日志降噪）、**7.3**（local_only 播放）、**8.1**（hls.js 按需加载）、**8.2/8.3**（自适应轮询 + fetch 超时）、**8.4**（密码框不回填）、**10.3**（旧库迁移测试）。

> 仍未做：1.1 代码层默认强制（本次仅线上启用，默认值仍为关闭）、2.1 NVR 密码轮换、3.4 失败段单段退避上限、5.3/5.4 SQL 聚合优化、7.1/7.2 转码与切片、9.x 可观测性。

## 实测现状

| 指标 | 数值 / 现象 |
|---|---|
| 索引规模 | 约 1.3 万段；done 4,005、failed 3,726、pending 483、skipped 4,960 |
| 失败原因 | 3,726 条 failed **全部**是 `get_user_id: error_code=-40401`（NVR 繁忙 / 会话不足） |
| 录像占用 | `rec/` 约 **541 GB**；`/data` 3.0 TB 中剩余 2.18 TB |
| 通道 | 实际只有 cam1–cam6；配置里 channels 为 `[1,1,2,2,…,8,8]`（重复） |
| 保留策略 | `retention_days=0`，从不自动删除 |
| 登录保护 | `auth_enabled=false`；`GET /api/settings` 直接返回明文 `nvr_pass` |
| 日志 | 线上库 `logs` 表**没有 `ch` 列**；当前代码的 `ensureLogChannel` 从未被调用 |
| 前端体积 | `app.js` 99,879 B、`style.css` 40,192 B、`hls.min.js` 619,692 B；**无 gzip/br** |
| 轮询 | 每 **2 秒**请求 `/api/sync/status`，空闲时也不降频 |
| 部署 | 线上版本落后 HEAD 多个提交；`container-nvrsync.service` 直接跑 `:main` |

---

## 1. 安全与访问控制（P0，7 点）

| # | 优化点 | 证据 / 说明 | 优先级 | 工作量 |
|---|---|---|---|---|
| 1.1 | **默认开启登录保护**，或在首次公网访问时强制进入“设置管理员密码”流程 | `auth_enabled=false` 时任何人都能读写设置、触发同步；线上当前就是该状态 | P0 | M |
| 1.2 | **`GET /api/settings` 不再返回明文 NVR 密码**；只返回 `nvr_pass_set:true`，PUT 时空值表示“保持不变” | `handleGetSettings` 直接序列化 `store.Settings`，`NVRPass` 在 `store.go:68` | P0 | S |
| 1.3 | **修复反向代理下的登录限速**：当前按 `RemoteAddr` 计数，所有请求都来自 traefik，一个攻击者即可锁死所有人 | `clientIP()` 明确忽略 `X-Forwarded-For`（`auth.go:424`）；需可信代理白名单 + 正确取客户端 IP | P0 | M |
| 1.4 | **Cookie 加 `Secure`**（HTTPS 时）并考虑 `__Host-` 前缀 | `setSessionCookie` 只设了 HttpOnly/SameSite（`auth.go:391`） | P1 | S |
| 1.5 | **补齐安全响应头**：CSP、X-Content-Type-Options、X-Frame-Options、Referrer-Policy、Permissions-Policy、HSTS | 线上实测全部缺失；traefik 也未统一加 | P0 | S |
| 1.6 | **给写接口加 CSRF 防护**：校验 `Origin`/`Sec-Fetch-Site`，并要求 `Content-Type: application/json` | `decodeJSON` 不检查 Content-Type；目前只靠 SameSite=Lax | P1 | S |
| 1.7 | **登录/会话可观测**：记录失败用户名（不记密码）、会话创建/失效、支持“退出全部设备” | 现在只有 `auth` 日志，且日志表没有通道/会话维度 | P2 | M |

## 2. 密钥与配置治理（P0，5 点）

| # | 优化点 | 证据 / 说明 | 优先级 | 工作量 |
|---|---|---|---|---|
| 2.1 | **轮换 NVR 管理员密码**，并同步更新 nvr2rtc/frigate 等消费者 | `scripts/smoke.sh` 把 `ad123min` 作为默认密码提交进了公开仓库；线上配置也是同一个 | P0 | S（运维） |
| 2.2 | 从 `scripts/smoke.sh` 删除硬编码密码，改为 `PASS` 必填或 `NVRSYNC_SMOKE_PASS` | `PASS=${PASS:-ad123min}`（第 8 行） | P0 | S |
| 2.3 | 支持从环境变量/secret 文件读取 NVR 密码，避免落盘明文 | `config.json` 里 `nvr_pass` 明文（0600，但仍在磁盘/备份中） | P1 | M |
| 2.4 | 配置写入前做**规范化**：通道去重排序、`cutoff` 枚举校验、`catalog_days/minutes` 上限、通道数上限 | 线上出现 `[1,1,2,2,…]`；`validateSettings` 只校验了少数字段（`api.go:180`） | P1 | S |
| 2.5 | 设置变更写**审计日志**（谁、哪些字段变了，敏感值脱敏） | 现在只写一条 `settings updated`（`api.go:174`） | P2 | M |

## 3. 同步可靠性与 NVR 会话（P0，8 点）

| # | 优化点 | 证据 / 说明 | 优先级 | 工作量 |
|---|---|---|---|---|
| 3.1 | **把 `-40401` 当可重试的瞬态错误**：更长退避、重新登录、不会直接写 `failed` | 3,726 条 failed 全部是该错误；`downloadOne` 3 次尝试后直接 `markFailed` | P0 | M |
| 3.2 | **`GetUserID` 内部重试 + 重新登录**，而不是把错误抛给每个下载任务 | `client.go:225` 只尝试一次；并发任务共用同一 client | P0 | S |
| 3.3 | **忙时熔断**：连续 N 次 `-40401` 暂停本轮，保留 pending，避免把 NVR 打得更忙 | 失败日志显示 2 个 worker 同时重试，容易形成风暴 | P0 | M |
| 3.4 | **失败段退避/限制重试次数**：不要把 3,726 条 failed 每轮全量重试 | `collectQueue` 每轮收 `pending+failed+syncing`（`syncer.go:376`） | P1 | M |
| 3.5 | **对 catalog 与 sync 的 NVR 会话统一限流**：全局信号量，避免 catalog 登录与下载抢 session | catalog 自己 `new nvr.Client + Login`（`syncer.go:1089`） | P1 | M |
| 3.6 | **自动同步失败时指数退避**，并合并重复错误日志 | `AutoLoop` 每 15 秒重试并 `Log(error)`（`syncer.go:196`） | P1 | S |
| 3.7 | **下载前检查磁盘余量**，不足时暂停并给出明确错误 | 541 GB 已用、保留策略为 0；盘满时只会逐段失败 | P1 | S |
| 3.8 | **校验 NVR 声明大小与落盘大小**（允许少量偏差），提前发现截断 | `validateTSFile` 只检查 TS 对齐与 sync byte | P2 | S |

## 4. 数据一致性与存储生命周期（P1，7 点）

| # | 优化点 | 证据 / 说明 | 优先级 | 工作量 |
|---|---|---|---|---|
| 4.1 | **retention 同时处理 `local_only`**，否则 NVR 覆盖后的本地文件永远不会过期 | `cleanupRetention` 只遍历 `done`（`syncer.go:706`） | P0 | S |
| 4.2 | **retention 独立于 sync 运行**（定时/对账后），不再“只有同步过才清理” | 仅在 `run()` 末尾调用（`syncer.go:329`） | P1 | S |
| 4.3 | **清理空目录与孤儿 `.part`**；启动时扫描磁盘孤儿文件 | 崩溃后 `.part` 可能长期残留 | P2 | M |
| 4.4 | **磁盘与 DB 对账**：DB 丢失/恢复后，能重新索引已有 `.ts` 与 sidecar | 目前 DB 是唯一事实源，sidecar 只写不读 | P2 | L |
| 4.5 | **修正 catalog 的 “新增” 统计与重复 upsert**：skipped 段每轮都被当成新段写回 | `known` 只包含 pending/syncing/done/failed；skipped 不在内（`syncer.go:1109`） | P0 | S |
| 4.6 | **天聚合去重**：重叠的连续录像 + 事件片段会重复计时长/码率 | `Days` 直接累加所有 done 段（`store.go:456`）；播放列表才做 `dropContained` | P1 | M |
| 4.7 | **录制文件权限收紧**：目录 0755、文件 0644 对同机其他用户可读 | `MkdirAll(..., 0o755)`、`os.Create`（`syncer.go:566`） | P2 | S |

## 5. 数据库与查询性能（P1，6 点）

| # | 优化点 | 证据 / 说明 | 优先级 | 工作量 |
|---|---|---|---|---|
| 5.1 | **执行迁移**：打开旧库时调用 `migrate`，补 `logs.ch` 列 | `migrate` 定义了但 `Open` 从未调用（`sqlite.go:115`）；线上库确实没有该列 | P0 | S |
| 5.2 | **日志 INSERT 写入 `ch`** | `appendLog` 的 INSERT 只有 `(ts,level,kind,msg)`（`store.go:553`），`LogCh` 的通道被丢弃 | P0 | S |
| 5.3 | **`Days` 改为 SQL 聚合**，避免把全部 done 行读进 Go 再分组 | 当前全表扫描 + map（`store.go:456`） | P1 | M |
| 5.4 | **`Stats` 合并为 1–2 条 SQL**，并用条件聚合替代多轮全表扫描 | 现在按 status、MIN/MAX、SUM、COUNT(DISTINCT) 共 5 次查询（`stats.go:35`） | P1 | M |
| 5.5 | **队列查询合并**：一次查出多通道多状态的 pending/failed | `collectQueue` 对每个 ch×3 状态分页查询（`syncer.go:376`） | P1 | M |
| 5.6 | **分页避免 O(n²)**：`listChannel` 用 offset 递增，每页都重算 COUNT | `listChannel`/`listStatus`（`syncer.go:428`） | P2 | S |

## 6. 后端性能与资源（P1，6 点）

| # | 优化点 | 证据 / 说明 | 优先级 | 工作量 |
|---|---|---|---|---|
| 6.1 | **开启 gzip/br 压缩**（HTML/JS/CSS/JSON） | 线上 `app.js` 99 KB、`hls.min.js` 620 KB 均原样传输；无 `Content-Encoding` | P0 | S |
| 6.2 | **静态资源内容哈希 + 长缓存 + ETag/304** | `no-cache` 且 embed FS 无 ModTime/ETag，每次刷新都全量重传 | P1 | M |
| 6.3 | **下载 part 不再整块 `make([]byte,n)`**，改流式写入 + 增量 probe | 每 part 最多 64 MB，2 worker 峰值内存高（`download.go:246`） | P1 | M |
| 6.4 | **媒体请求先走规范路径**，避免每个 TS 都查一遍当天全部段 | `resolveMedia` 每次 day 查询 + 线性扫描（`api.go:504`） | P1 | S |
| 6.5 | **HTTP 请求日志降噪**：轮询/健康检查成功不打印，或采样 | 容器日志被 2 秒一次的 `/api/sync/status` 刷屏 | P1 | S |
| 6.6 | **媒体响应支持更细粒度缓存/ETag**，并考虑 HEAD 请求 | `ServeFile` 支持 Range，但无 ETag 控制 | P2 | S |

## 7. 播放与媒体体验（P1，6 点）

| # | 优化点 | 证据 / 说明 | 优先级 | 工作量 |
|---|---|---|---|---|
| 7.1 | **实现真正的转码代理**，或从 UI 移除 `transcode` 开关 | `EnsureProxy` 永远返回 `ErrNotSupported`（`play.go:152`），但设置页仍提供开关 | P1 | L |
| 7.2 | **按 6–10 秒切片生成 HLS**，不要用整小时 TS 作为单个分片 | `DayPlaylist` 直接把整段 TS 当一个 `#EXTINF`（`play.go:137`），seek/首播都很重 | P1 | L |
| 7.3 | **前端播放器识别 `local_only`** | 后端 playlist 包含 local_only，前端 `done` 只筛 `status==='done'`，时间轴与真实播放列表不一致 | P0 | S |
| 7.4 | **HEVC 兼容性提示前置**：进入播放器前检测 `MediaSource.isTypeSupported`，不要等黑屏 | 现在只有错误后才提示 | P2 | S |
| 7.5 | **增加下载/分享按钮**（原始 TS 或 m3u8），方便 VLC 用户 | 目前只展示播放列表地址 | P2 | S |
| 7.6 | **时间轴性能**：`pointermove` 每次都 `filter` 全部段，段多时卡顿 | `app.js` 时间轴 hover（约 1945 行） | P2 | S |

## 8. 前端性能与交互（P1，7 点）

| # | 优化点 | 证据 / 说明 | 优先级 | 工作量 |
|---|---|---|---|---|
| 8.1 | **按需加载 hls.js**：只在播放器且需要 MSE 时加载 | index.html 每个页面都同步加载 620 KB 的 `hls.min.js` | P0 | S |
| 8.2 | **状态轮询自适应**：运行中 2s、空闲 10s、隐藏页停止；用递归 `setTimeout` | 固定 `setInterval(...,2000)`（`app.js:695`） | P1 | S |
| 8.3 | **所有 fetch 加超时/AbortController**，超时后释放 `pollBusy` | `api()` 没有 timeout；一次挂起会让轮询永久卡住 | P1 | S |
| 8.4 | **设置页密码框不再回填明文**，改为“已保存，留空不修改” | `value: saved.nvr_pass`（`app.js` 设置表单） | P0 | S |
| 8.5 | **提供退出登录入口** | 启用登录后 SPA 里没有 logout 按钮 | P1 | S |
| 8.6 | **空/错/加载状态统一**，表格与卡片增加骨架屏一致性 | 已有部分骨架，仍有“加载中…”闪烁 | P2 | S |
| 8.7 | **移动端与可访问性**：时间轴触控、表格横向滚动、`prefers-reduced-motion` | 已有 focus-visible；缺 reduced-motion 与触控优化 | P2 | M |

## 9. 可观测性、部署与 CI（P2，7 点）

| # | 优化点 | 证据 / 说明 | 优先级 | 工作量 |
|---|---|---|---|---|
| 9.1 | **健康检查分层**：`/api/health` 只报存活；新增 `/api/ready` 检查 DB/存储/NVR | 现在只返回 `{ok,version}` | P1 | S |
| 9.2 | **暴露 Prometheus 指标**（同步段数、失败数、字节、NVR 错误、DB 大小） | 目前只能看日志/页面 | P2 | M |
| 9.3 | **结构化日志 + request-id + 级别** | 目前 `log.Printf` 纯文本，HTTP 与业务日志混在一起 | P2 | M |
| 9.4 | **容器资源限制与重启策略**：memory/cpu、StartLimit、失败告警 | systemd unit 只有 `Restart=on-failure`，无上限/告警 | P2 | S |
| 9.5 | **CI 增加 `govulncheck`/`staticcheck`/覆盖率**，并用 digest 固定基础镜像与 action | 当前只有 gofmt/vet/test | P2 | S |
| 9.6 | **镜像瘦身**：默认镜像装了 ffmpeg 但转码未实现；可拆 `-full` 标签 | `Dockerfile` 固定 `apk add ffmpeg` | P2 | M |
| 9.7 | **备份/恢复演练**：SQLite 在线备份（`VACUUM INTO`）+ 录制目录快照 | 541 GB 录像与 2.7 MB DB 都无明确备份策略 | P2 | M |

## 10. 文档、测试与产品化（P2，6 点）

| # | 优化点 | 证据 / 说明 | 优先级 | 工作量 |
|---|---|---|---|---|
| 10.1 | **修复文档漂移**：README 仍说“元数据 JSON”，实际已 SQLite；ARCH 的 Settings 已缺 `backfill_hours` 等 | `README.md:16`、`ARCH.md` | P1 | S |
| 10.2 | **补 HTTP 层集成测试**：设置脱敏、CSRF、迁移、catalog 幂等、retention | 目前 API 测试集中在 auth | P1 | M |
| 10.3 | **补迁移测试**：用旧 schema 建库 → Open → Logs(ch) 可用 | 线上正是旧 schema，当前会静默失败 | P0 | S |
| 10.4 | **补 NVR 故障注入测试**：`-40401`、超时、截断、部分 part | 现在的 nvr 测试偏协议解析 | P1 | M |
| 10.5 | **加 LICENSE、CHANGELOG、SECURITY.md** | 仓库无 LICENSE | P2 | S |
| 10.6 | **版本语义化**：health 返回 `version+commit+build_date`，镜像 tag 用 semver + digest | 现在 health 返回 commit SHA，部署用 `main` 浮动 tag | P2 | S |

---

## 建议的落地顺序

1. **先修部署阻塞与数据正确性**：5.1、5.2、4.5、4.1、7.3、8.4（否则新镜像上线后日志/对账仍有问题）。
2. **再修线上最大痛点**：3.1、3.2、3.3、3.4（3,726 条 `-40401` 失败），并给自动同步加退避。
3. **安全加固**：1.2、1.3、1.5、1.6、2.1、2.2；确认后再开 1.1。
4. **性能与体验**：6.1、8.1、8.2、8.3、6.5、6.4。
5. **中长期**：7.1/7.2、4.4、5.3/5.4、9.2/9.3。

> 说明：线上后端在 NAS（192.168.0.22）而不是 LXC100 容器内；LXC100 的 traefik 只是入口。
> 任何涉及 NVR 密码轮换、开启登录保护、retention 删除范围的操作，都应先确认影响面。
