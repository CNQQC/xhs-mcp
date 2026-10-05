# MCP 研究接口与诊断契约

此文描述 MCP 投影返回，不要与 REST 原始数据字段混用。所有新增字段均为增量字段；原有工具参数、`content` 文本、`ref`、批量输入顺序与部分成功行为保留。

## 先确认服务，再检查登录

- `get_diagnostics` 无参数，读取进程与构建信息，不开浏览器、不连接小红书、不读取或返回 cookies/token，不验证登录或页面可用性。
- `/health` 也是进程存活检查。HTTP 200 或诊断成功不等于账号已登录，也不代表客户端到服务端的所有网络路径正常。
- MCP `initialize` 的 `serverInfo.version` 与健康/诊断信息使用同一构建来源，保留 `-ldflags "-X main.version=..."` 发布方式；开发构建使用可获得的 Go VCS 信息，缺失时明确为开发版。
- 真正需要确认账号状态时再调用 `check_login_status`。登录失败、安全验证、页面超时应按错误建议处理，不能把任意抓取失败都当成登录过期。

## 稳定身份与可追溯来源

`search_feeds`、`list_feeds`、用户主页中的笔记及详情提供：

- `note_id`：站点提供的稳定笔记 ID，适合去重、引用和比较
- `source_url`：仅在笔记 ID 与实际访问令牌可安全构成站点链接时提供；缺数据时省略，不合成令牌或虚构可访问性
- `retrieved_at`：本次结果投影时间，UTC RFC3339，不是原帖发布时间
- `ref_expires_at`：当前返回 `ref` 的到期时间，UTC RFC3339

`source_url` 中的访问令牌沿用站点访问模型，链接可能过期或要求登录，不保证公开永久可用。不要把带令牌的链接当作无敏感参数的公共永久链接；没有另行输出独立 token 字段。稳定 ID 也不能替代工具调用需要的 ref/token。

`ref` 是进程内临时句柄：通常一小时有效，服务重启即失效，容量淘汰也可能使其提前失效。重复搜索会得到不同 ref；不要用 ref 当数据库主键。批量详情带回输入 ref，其到期时间不会因抓取被延长。

## 搜索边界与研究流程

列表返回的 `coverage` 包含 `source`、`scope: first_page`、`returned_count`、`continuation_supported: false`、`exhaustive: false`。

当前没有受支持的搜索游标或下一页参数。返回零条或一页结果不能证明站点上没有其他匹配内容；不要声称穷尽检索。可使用不同关键词及已支持的筛选项拓宽样本，然后按 `note_id` 去重。

建议流程：

1. 搜索得到标题、作者、互动数、稳定 ID 和 ref
2. 按相关性挑选最多六个 ref，用 `get_feed_details` 批量取正文
3. 批量只取首屏评论；相同笔记 ID 与令牌的重复项只抓一次，仍按输入顺序返回全部项
4. 根据 `coverage` 与 `warnings` 决定是否对单条调用 `get_feed_detail`，显式设置 `load_all_comments`、`limit`、`click_more_replies`、`reply_limit`
5. 图片地址仅在 `include_images=true` 时返回；要实际看图，按需调用 `get_feed_image`

批量最多六项、最多两个并发标签页；已有页面、字幕下载与评论滚动预算仍生效。`load_all_comments=true` 是有预算的滚动请求，不是完整抓取承诺；`limit` 是加载目标，站点批次可能造成超过目标的返回数量。没有新增自动翻页或无上限重试。

## 详情完整性

MCP 字幕字段为 `subtitle` 和 `subtitleLang`；REST 对应 `video.subtitleText`。默认评论数量由首屏实际加载情况决定，不保证恰好十条。

`coverage.comments` 区分请求模式、已返回一级评论/回复数量、实际 `has_more_top_level`、首批请求是否完成、已知的完整性和请求预算。缺少可靠信号的完整性字段省略，表示未知；不要把省略当作 true。回复完整性只针对已返回父评论，不能代表所有未加载评论。

`coverage.subtitle.status` 区分 `available`、`unavailable`、`failed`、`partial`、`not_applicable` 和 `unknown`。`available` 表示所选字幕文件已成功处理，不能证明视频所有语音都被站点字幕覆盖。字幕为空不是视频没有内容的证据。

评论等待/滚动或字幕下载/解析发生软失败时，正文可继续成功返回，但 `warnings` 会明确提示，完整性也不会被标成完整。

## 请求与错误

MCP 调用结果增加结构化诊断数据，含服务端生成的 `request_id`，便于和服务日志对应；原有 `content` 继续可读。错误包含 `code`、`message`、`retryable`、`next_action`、`request_id`。

常见代码：

| code | 处理方向 |
| --- | --- |
| `INVALID_ARGUMENT` | 修正参数，再调用 |
| `REF_EXPIRED` | 重新搜索/取列表，使用新 ref；不要原样循环重试 |
| `LOGIN_REQUIRED` | 检查登录状态，必要时由账号本人登录 |
| `RISK_VERIFICATION_REQUIRED` | 获取验证二维码，由账号本人完成验证 |
| `RATE_LIMITED` / `BROWSER_BUSY` | 减少并发、等待后有界重试 |
| `TIMEOUT` | 缩小批次/评论预算，必要时检查网络后有界重试 |
| `CANCELLED` | 调用已取消；由调用方决定是否重新发起 |
| `UPSTREAM_ERROR` / `INTERNAL_ERROR` | 结合 request_id 排查，不盲目反复调用 |

`retryable` 是故障恢复提示，不是自动执行或重复写操作的授权。发布、评论等写操作出现不确定结果时应先核实实际状态，避免重复执行。

批量失败项保留 `error` 文本并新增 `error_info`。部分失败仍返回可用成功项；全部失败时工具 `isError=true`。失败项的 `note_id` 仅在 ref 成功解析时可提供。

## 副作用与验证范围

`list_notifications` 打开分区会标记通知已读，因此 `readOnlyHint=false`；工具说明也应体现此行为。

本阶段不改变发布、定时发布、私密发布或账号安全机制，不声称解决某个客户端的 Python/Muse 连接错误。连接故障需要对应客户端日志、服务日志和 request_id 才能定位。

离线验证：`go build ./...`、`go vet ./...`、`go test ./...`。默认测试不应执行真实发帖、点赞或登录。真实站点抓取、会话状态与部署后的网络连通性另行验证，不能由离线通过代替。
