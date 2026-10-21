# nvrsync

TP-Link NVR 录像 → 本地/NAS 定时同步 + 按天在线回看。**单二进制**（Go + 内嵌前端），无外部依赖（可选 ffmpeg 转码）。

> 配套项目：[nvr2rtc](https://github.com/myml/nvr2rtc)（实时预览转 HTTP-TS）。
> 协议细节见 nvr2rtc 仓库的 `PROTOCOL.md` / `DLFILE.md`。

## 特性

- **自动对账**：后台每 5 分钟把 NVR 当前持有的录像合并进列表；NVR 已覆盖但本地有文件的标为「仅本地」
- **同步**：按通道、按时间段增量拉取 NVR 录像，实测可达 **45–52× 实时**（瓶颈是百兆网口）
- **录像**：按天聚合，24 小时时间轴，点击即播（HLS，浏览器 hls.js）
- **设置**：NVR 地址/账号、存储目录、通道、自动同步、回填、保留策略、转码开关
- **同步截止**：可选「最新 / 昨天 / N 天前」，只同步已封盘录像，不会碰到正在写入的段
- **零依赖**：前端资源 `go:embed` 进二进制；元数据 JSON 原子写，sidecar 随录像落盘

[![docker](https://github.com/myml/nvrsync/actions/workflows/docker.yml/badge.svg)](https://github.com/myml/nvrsync/actions/workflows/docker.yml)

## 快速开始

### Docker

```bash
docker run -d --name nvrsync \
  -p 8099:8099 \
  -v /srv/nvrsync:/data \
  --restart unless-stopped \
  ghcr.io/myml/nvrsync:latest
```

镜像支持 `linux/amd64` 与 `linux/arm64`；`/data` 里是 `config.json`（设置）、`nvrsync.db`（SQLite：录像段/日志/同步状态）和 `rec/`（录像文件）。

NAS（docker compose）：

```bash
mkdir -p /volume1/docker/nvrsync && cd /volume1/docker/nvrsync
curl -fsSLO https://raw.githubusercontent.com/myml/nvrsync/main/deploy/docker-compose.yml
docker compose up -d
```

> GHCR 包默认可能是私有的：若 `docker pull` 失败，先在 GitHub 的 Package 设置里把 `nvrsync` 改为 public，或在 NAS 上 `docker login ghcr.io`（用带 `read:packages` 的 token）。

### 本地构建

```bash
go build -o nvrsync ./cmd/nvrsync
./nvrsync -listen :8099 -data ./data
# 打开 http://localhost:8099 ，在「设置」里填 NVR 地址和密码，保存后去「同步」点开始
```

环境变量：`NVRSYNC_LISTEN`、`NVRSYNC_DATA`。

## 存储布局

```
data/
  config.json                    # 设置（NVR 地址/账号/通道/同步策略）
  nvrsync.db                     # SQLite：录像段、日志、同步状态
  rec/cam1/2026-10-21/07-00-00_08-00-00.ts        # 录像
  rec/cam1/2026-10-21/07-00-00_08-00-00.ts.json   # sidecar
  cache/hls/...                  # 转码缓存（可选）
```

## 状态

开发中（v0.1）。接口契约见 `ARCH.md`。
