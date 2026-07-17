# MelodyShare

单用户文件上传分享工具。Go 单二进制，前端已嵌入，元数据存 SQLite，文件存本地磁盘或
Cloudflare R2。为部署在 Cloudflare 代理之后设计：**客户端自动分片上传**（默认按文件
大小自适应 5–95MB/片、分片数量随之自动确定），任意大小的文件都不会触碰 Cloudflare
100MB 请求体限制；下载支持 Range 断点续传。

## 功能

- 单用户登录（HttpOnly 会话 Cookie，30 天）；防爆破：登录尝试串行化，
  连续失败 5 次后指数锁定（2s 起翻倍，封顶 5 分钟），锁定期内直接 429
- 拖拽/多选上传，多线程分片上传（默认自适应并发：吞吐仍在提升就加线程、
  出错或回落就退避，1–8 线程；也可手动固定），失败自动重试，实时进度；
  所有文件共享全局并发上限，HTTP/1.1 下预留一条连接给页面操作，上传中界面不卡顿
- 断点续传：上传中断后重新选择同一文件，自动跳过已完成的分片继续；支持 Ctrl+V 粘贴上传
- 分享短链 `/f/{slug}`（可自定义）打开的是预览页：图片/视频/音频/PDF/文本在线预览，
  点击按钮才下载；直链下载走 `/f/{slug}/dl`（curl/wget 可用）
- 可选过期时间、下载密码、下载次数上限（阅后即焚；设了上限的文件自动禁用在线预览）
- 文件管理页：复制链接、下载计数、修改过期/密码/次数上限、删除、存储用量与剩余磁盘展示
- 可选 Cloudflare R2 存储：**上传与下载都不经过 VPS**——浏览器经预签名 URL 直传，
  下载 302 到预签名 GET（R2 egress 免费）；未配置 CORS 时上传自动回退为服务器中转
- 磁盘余量不足 1GB 时拒绝新的本地上传，而不是写到一半失败
- 登录后的**设置页**：站点名称、Base URL、分片大小、R2 配置、用户名/密码都可以在
  网页上改，保存后**立即热生效，无需重启**（改密码需确认当前密码，已登录会话不受影响）
- 会话持久化（重启不掉登录，数据库只存 token 哈希）；安全响应头（CSP 等）
- 优雅关闭（SIGTERM 后等待进行中的请求完成）
- 免登录**网络剪切板** `/p`：网页或命令行粘贴文本得短链，默认 2 小时过期（最长 24 小时），
  到期自动删除；短链对浏览器显示带复制按钮的页面、对 curl 直接输出原文；
  单条上限与总开关可在设置页调整，另有每 IP 每小时 30 条的频率限制
- 后台自动清理过期文件与超过 72 小时未完成的上传

## 快速开始

```sh
# 二进制
SHARE_PASSWORD=your-password ./share
# 或 Docker
docker compose up -d   # 先改 docker-compose.yml 里的 SHARE_PASSWORD
```

打开 `http://localhost:8080`，用 `admin` / 你设置的密码登录。

## 配置（环境变量）

除 `SHARE_ADDR` 和 `SHARE_DATA_DIR` 外，下列配置都可以在登录后的设置页修改；
**网页上保存过的设置存入数据库并优先于环境变量**（此后改环境变量不再生效）。
`SHARE_PASSWORD` 只在首次运行（尚未在设置页保存过密码）时必填。忘记密码时删除
数据库中的记录即可回退到环境变量：
`sqlite3 data/share.db "DELETE FROM settings WHERE key='password_hash'"`。

| 变量 | 默认 | 说明 |
|---|---|---|
| `SHARE_ADDR` | `:8080` | 监听地址 |
| `SHARE_DATA_DIR` | `./data` | 数据目录（数据库、文件、密钥） |
| `SHARE_SITE_NAME` | `MelodyShare` | 站点名称（页面标题/品牌） |
| `SHARE_USERNAME` | `admin` | 登录用户名 |
| `SHARE_PASSWORD` | — | 登录密码，**首次运行必填** |
| `SHARE_BASE_URL` | 按请求推断 | 分享链接前缀，如 `https://share.example.com` |
| `SHARE_CHUNK_SIZE_MB` | `auto` | 分片大小：`auto` 按文件大小自适应（5–95，分片数随之确定），或固定 5–95 |
| `SHARE_R2_ENDPOINT` | — | `<accountid>.r2.cloudflarestorage.com` |
| `SHARE_R2_ACCESS_KEY` / `SHARE_R2_SECRET_KEY` / `SHARE_R2_BUCKET` | — | R2 凭据与桶，四项全配才启用 |

## 网络剪切板

免登录的临时文本分享，适合手机 ↔ 电脑互传文本、或在终端里快速分享命令输出：

```sh
# 粘贴（默认保留 2 小时，返回短链）
echo "hello" | curl --data-binary @- https://s.example.com/p

# 指定保留时间（1 分钟到 24 小时）
curl --data-binary @notes.txt "https://s.example.com/p?ttl=24h"

# 取回（curl 得到原始文本；浏览器打开则是带复制按钮的页面）
curl https://s.example.com/p/ab3k9
```

浏览器直接打开 `/p` 即可使用网页版。注意 `curl -d` 会丢掉换行，请始终用
`--data-binary`；内容须为 UTF-8 文本，二进制请走登录后的文件分享。

## R2 直传的 CORS 配置

启用 R2 后，浏览器会把分片直接 PUT 到 `*.r2.cloudflarestorage.com`（预签名 URL），
需要在 R2 桶的设置里加一条 CORS 规则，否则自动回退为经 VPS 中转：

```json
[
  {
    "AllowedOrigins": ["https://share.example.com"],
    "AllowedMethods": ["PUT"],
    "AllowedHeaders": ["*"],
    "ExposeHeaders": ["ETag"],
    "MaxAgeSeconds": 3600
  }
]
```

`ExposeHeaders: ["ETag"]` 是必需的——前端要读取每个分片的 ETag 才能完成合并。

## 部署到 VPS（Cloudflare 代理）

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o share .
```

1. 上传二进制到 VPS，用仓库里的 `deploy/share.service`（内含完整安装步骤注释）配置 systemd
2. Nginx/Caddy 反代到 `127.0.0.1:8080`（或直接监听 `:80` 交给 Cloudflare 回源），
   注意反代需放行大请求体（Nginx: `client_max_body_size 100m;`）并透传 `X-Forwarded-Proto`
3. Cloudflare DNS 开橙云代理即可；分片始终小于 100MB，无需调整任何 CF 设置

## 开发

```sh
go test ./...
SHARE_PASSWORD=dev go run .
```
