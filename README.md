# nvrsync

TP-Link NVR 录像 → 本地/NAS 定时同步 + 按天在线回看。**单二进制**（Go + 内嵌前端），无外部依赖（可选 ffmpeg 转码）。

> 配套项目：[nvr2rtc](https://github.com/myml/nvr2rtc)（实时预览转 HTTP-TS）。
> 协议细节见 nvr2rtc 仓库的 `PROTOCOL.md` / `DLFILE.md`。

## 特性

- **同步**：按通道、按时间段增量拉取 NVR 录像，实测可达 **45–52× 实时**（瓶颈是百兆网口）
- **录像**：按天聚合，24 小时时间轴，点击即播（HLS，浏览器 hls.js）
- **设置**：NVR 地址/账号、存储目录、通道、自动同步、回填、保留策略、转码开关
- **同步截止**：可选「最新 / 昨天 / N 天前」，只同步已封盘录像，不会碰到正在写入的段
- **零依赖**：前端资源 `go:embed` 进二进制；元数据 JSON 原子写，sidecar 随录像落盘

## 快速开始

```bash
go build -o nvrsync ./cmd/nvrsync
./nvrsync -listen :8099 -data ./data
# 打开 http://localhost:8099 ，在「设置」里填 NVR 地址和密码，保存后去「同步」点开始
```

环境变量：`NVRSYNC_LISTEN`、`NVRSYNC_DATA`。

## 存储布局

```
data/
  store.json                     # 元数据（段状态/日志/设置）
  rec/cam1/2026-10-21/07-00-00_08-00-00.ts        # 录像
  rec/cam1/2026-10-21/07-00-00_08-00-00.ts.json   # sidecar
  cache/hls/...                  # 转码缓存（可选）
```

## 状态

开发中（v0.1）。接口契约见 `ARCH.md`。
