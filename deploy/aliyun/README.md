# 阿里云人工扫码验证入口

2026-09-08 配置修复。未修改应用源码、二进制、现有 cookies 或鉴权 token。

## 定位与根因

- 公网主机：阿里云 `8.130.164.28`，入口为 Nginx 1.26.2。
- Compose：`/opt/xiaohongshu-mcp/docker-compose.yml`；容器 `xiaohongshu-mcp`，`restart: unless-stopped`。
- 二进制：宿主机 `/opt/xiaohongshu-mcp/app-patched` 只读挂载为 `/app/app`。构建提交 `65d1914da605b4720e517c6dc6571d141617798a`，`vcs.modified=false`，与本仓库修复前 HEAD 一致。
- 应用监听容器内 `:18060`；宿主机仅发布 `127.0.0.1:18060`。Mac 上旧 LaunchAgent 已禁用。
- cookies 位于宿主机 `/opt/xiaohongshu-mcp/data/cookies.json`，挂载到 `/app/data/cookies.json`。
- `XHS_PUBLIC_BASE_URL` 去掉末尾所有 `/`，再拼 `/verify/<token>`。允许 `https://host[/prefix]`，不能带查询串或 fragment；不要把 `/verify` 本身写进 base。
- `routes.go` 在同一个 Gin HTTP server 上提供验证页与状态接口；页面没有额外进程或端口。
- 仅依赖 `GET /verify/<token>` 和 `GET /verify/<token>/state`。CSS/JS 内嵌，二维码为状态 JSON 的 `qr` data URI，无外部静态资源。
- token 为 32 随机字节的 base64url 无填充编码（43 字符），有效期 15 分钟。页面每 2 秒轮询状态。首次打开前会话宽限 90 秒，打开后心跳宽限 3 分钟，单个浏览器会话绝对上限 10 分钟。
- Nginx 的 `/x/<长期 token>/mcp` 规则精确匹配结尾 `/mcp`，校验路径 token 后重写为 `/mcp` 并注入后端 Bearer。它不是任意子路径的代理前缀。
- 访问日志确认 2026-09-08 10:16:00 的 `/x/<长期 token>/verify/<临时 token>` 和 10:16:14 的 `/verify/<临时 token>` 均在 Nginx 返回 404（555 字节）；对应后端时段无请求记录。

## 持久化改动

1. 新建服务器 `/etc/nginx/snippets/xhs-verify.conf`，内容见同目录 `xhs-verify.conf`。仅将 43 字符 token 的 HTML 与 `/state` GET 请求原路径转发给 `127.0.0.1:18060`；其余 `/verify/` 子路径返回 404，非 GET 返回 405。禁用此路径访问日志及可能含 token 的错误日志，禁用代理缓存与缓冲。
2. 在 `/etc/nginx/conf.d/aimforge.conf` 的 `api.zhangzr.xyz` TLS server 内添加 `include /etc/nginx/snippets/xhs-verify.conf;`。
3. 在 `/opt/xiaohongshu-mcp/docker-compose.yml` 的 environment 添加 `XHS_PUBLIC_BASE_URL=https://api.zhangzr.xyz`。

执行 `nginx -t`、`docker compose config --quiet` 通过后，使用 `docker compose up -d --no-deps --pull never xiaohongshu-mcp` 重建容器以应用新环境变量，再 `systemctl reload nginx`。单纯 `docker compose restart` 不会应用新环境变量。

最终链接格式：`https://api.zhangzr.xyz/verify/<43 字符临时 token>`。

原有 `/xhs/` Bearer 入口及 `/x/<长期 token>/mcp` 保留。新验证入口依靠应用自带的短期随机 token，只开放两条必要路径，不需要公开长期 MCP token。

## 已完成验收

- 通过 SSH 在服务器内向 `GET /api/v1/verification/qrcode` 发送现有 Bearer 凭证，返回 `blocked=true`、`verify_type=124`、`verify_biz=461`、完整 `verify_url` 和二维码。未把 Bearer 输出到验收记录。
- 从服务器外的 Mac 使用公网 DNS、正常 TLS 校验和 `curl --noproxy '*'` 访问新 token URL：200，`text/html; charset=utf-8`，HTML 标题为“小红书安全验证”。响应头见 `verification-response-headers.txt`。
- 公网 `/state` 返回 `pending` 和可显示的二维码 data URI。真实浏览器以 390 × 844 视口显示二维码与“等待扫码”。
- 同一 token 的二维码从 8330 字符变成 8430 字符，SHA-256 从 `65a94324ff5aefd2296a30aa58b464b2c202e4b09de74dab16a7b0d3603e4587` 变为 `f0ea737d8320fc77d252ddc8fa65016777cf0d12d1f4ae5b31fdb0a740d9a9da`。服务日志同时记录会话 #1 “二维码已过期，就地点击换了一张新码”，证明公网页面取得新码。
- `/mcp`、`/mcp/`、`/api/v1/verification/qrcode` 无凭证均 404；`/xhs/mcp`、`/xhs/api/v1/verification/qrcode` 无 Bearer 均 401；错误长期 token 的 MCP 路径 403；验证页多余子路径 404。
- 原正确长期 token 的 MCP `tools/list` 正常返回 19 个工具。
- 重建后 cookies 的 SHA-256、mtime、大小均与操作前一致。未替换、清空或手工写入 cookies。
- 随后人工扫码完成，`/state` 返回 `verified` 和“验证已通过，登录状态已保存”。日志确认“安全验证已通过，cookies 已保存，会话 #1”；此后 cookies 的校验值发生变化，属于验证流程正常落盘。
- 使用实际小红书 MCP 连接器执行 `search_feeds({"keyword":"美食"})`，13.9 秒返回 `isError=false`、`count=20`，无安全验证拦截，完整业务流程验收通过。
- 新生成的验证 token 在 Nginx access.log 中出现次数为 0；`docker`、`nginx` systemd 服务均为 enabled。

公网验收命令（用刚生成的 `verify_url` 替换 URL）：

```sh
curl --noproxy '*' --fail-with-body --silent --show-error --max-time 20 \
  -D - -o /tmp/xhs-verify-page.html 'https://api.zhangzr.xyz/verify/<token>'
```

人工扫码、cookies 自动落盘与 `search_feeds` 恢复已完成。网络验收来自服务器外的 Mac 公网 HTTPS 请求和真实浏览器；没有独立测量用户手机是否使用蜂窝网络。聊天截图的二维码不会更新；应打开验证链接读取当前二维码。只有一部手机时，可截取页面当前二维码，再到小红书 App 的扫一扫从相册选取。

## 回滚

原文件备份在服务器 `/opt/xiaohongshu-mcp/backup-verify-20260908-102250/`（目录权限 0700，含配置与 cookies 校验值，不含 cookies 副本）。在服务器运行：

```sh
cp -p /opt/xiaohongshu-mcp/backup-verify-20260908-102250/aimforge.conf /etc/nginx/conf.d/aimforge.conf
cp -p /opt/xiaohongshu-mcp/backup-verify-20260908-102250/docker-compose.yml /opt/xiaohongshu-mcp/docker-compose.yml
nginx -t && systemctl reload nginx
cd /opt/xiaohongshu-mcp
docker compose config --quiet && docker compose up -d --no-deps --pull never xiaohongshu-mcp
```

恢复后的 Nginx 不再 include 新 snippet，可保留它作为记录。回滚同样不涉及 cookies、二进制或原有鉴权配置；重建容器会使已发临时验证链接失效。
