# 评论发布时间修复部署

2026-09-08 已部署至阿里云 `/opt/xiaohongshu-mcp` 的 `xiaohongshu-mcp` 服务。

- 版本：`14854e0-comment-time-20260908`，基于提交 `14854e043ecc95d2e6088046aa408f722f787c14` 加本地评论时间修复。
- MCP `get_feed_detail` 的评论及各层 `replies` 返回 `time`，格式为 `YYYY-MM-DD HH:mm`（UTC+8）；日期缺失时省略。
- 二进制：`/opt/xiaohongshu-mcp/app-patched`，容器内为 `/app/app`。
- SHA-256：`9468c667e3ed1066a246202cc605e45628aaeca5e8f156f633bff41ad1a058b0`，上传后及容器内均已核对。
- 旧程序备份：`/opt/xiaohongshu-mcp/backup-comment-time-20260908-vWf6A7/app-patched`。
- 使用现有 Compose 配置重建单个服务，使文件挂载指向替换后的程序。

验收：`/health` 返回上述版本及 `healthy`，容器运行且重启计数为 0。通过实际 MCP 连接器调用 `search_feeds` 与 `get_feed_detail` 成功；详情返回 10 条一级评论及 6 条子回复，全部带有可读 `time`。例如一级评论时间 `2026-07-28 08:45`，其子回复时间 `2026-07-28 09:46`。

如需回滚，在服务器执行：

```sh
cd /opt/xiaohongshu-mcp
cp -p backup-comment-time-20260908-vWf6A7/app-patched app-rollback-comment-time
mv app-rollback-comment-time app-patched
docker compose up -d --no-deps --force-recreate --pull never xiaohongshu-mcp
```

服务重启后旧 ref 失效，重新搜索或读取列表获取新 ref。
