# xhs-mcp

MCP for 小红书 / xiaohongshu.com。让你的 AI 助手直接访问小红书数据。

> ### 本项目衍生自 [xpzouying/xiaohongshu-mcp](https://github.com/xpzouying/xiaohongshu-mcp)
>
> **绝大部分代码、全部核心架构和产品设计都出自上游作者 [@xpzouying](https://github.com/xpzouying)
> 与该项目 29 位贡献者之手。** 本仓库是在上游 **v2.5.0**（commit
> [`6583124`](https://github.com/xpzouying/xiaohongshu-mcp/commit/6583124)，2026-08-13）
> 基线上做的一批稳定性修复与功能增补，不是重写，也不是替代品。
>
> **要用请先去[上游仓库](https://github.com/xpzouying/xiaohongshu-mcp)点 Star**，
> 也请通过上游的[赞赏渠道](https://github.com/xpzouying/xiaohongshu-mcp#赞赏支持)支持原作者
> —— 那里已经把收到的[善款捐了出去](https://github.com/xpzouying/xiaohongshu-mcp/blob/main/DONATIONS.md)。
> 本仓库不接受任何形式的赞赏。
>
> 上游以 Apache License 2.0 授权，本仓库沿用同一协议，见 [LICENSE](LICENSE)、[NOTICE](NOTICE)。

## 为什么会有这个仓库

下面这些改动已经按主题拆成 5 个 PR 提给上游（[#823](https://github.com/xpzouying/xiaohongshu-mcp/pull/823)、
[#824](https://github.com/xpzouying/xiaohongshu-mcp/pull/824)、
[#825](https://github.com/xpzouying/xiaohongshu-mcp/pull/825)、
[#826](https://github.com/xpzouying/xiaohongshu-mcp/pull/826)、
[#827](https://github.com/xpzouying/xiaohongshu-mcp/pull/827)），目前还都开着。

开源项目的维护节奏由维护者定，这很正常。但这些修复解决的是把服务放在容器里跑就会撞上的问题
（超时、panic、OOM），自己用着有效，就先在这里公开出来，给同样卡在这些问题上的人一条现成的路。

**上游一旦合并对应的 PR，本仓库会同步跟进；全部合并后就没有继续存在的必要，届时归档。**

## 相对上游的改动

改动都基于同一台目标机器（2 核 / 容器内存上限 1200MB）的实测，下面的数字都是实际量出来的，
不是估算。

### 1. 页面等待一律给上限，不再吃满 deadline 再 panic

rod 的 `MustWaitLoad` / `MustWaitStable` / `MustWaitDOMStable` **自身不带超时上限**
（官方注释写明 "d is not the max wait timeout"），唯一的退出路径是外层 page deadline。
而小红书的页面有懒加载、心跳信标、视频播放动画，「DOM 零变化 + 网络空闲 + load 完成」
这个条件实测根本达不成——于是只能一路耗到 deadline 然后 panic。

全仓库审计出 12 处同类隐患，其中最狠的几处：

| 位置 | 后果 |
|---|---|
| `comment_feed.go:33` | 视频笔记发评论**必定**卡满 60s 后 panic，一条都发不出去 |
| `feed_detail.go` | 视频笔记详情 deadline 是 10 分钟，一路耗满 |
| `login.go:82/99` | `Context(ctx)` 只换 context 没设 Timeout，**完全没有上限** |

新增 `xiaohongshu/wait.go` 立两条规矩：「等页面安静」一律是尽力而为的缓冲（8 秒上限、
超时只告警继续）；真正的就绪条件永远是后面那句具体的元素查找或数据判断。

**这里有个配套的坑**：把硬等降级成软超时，会用假阴性换掉 panic。原来的 `MustWaitStable()`
虽然会 panic，但它一旦成功、数据必然到位；换成软超时后，后面那句就绪判断就成了唯一防线。
而当时搜索路径后面跟的是 `MustWait(() => window.__INITIAL_STATE__ !== undefined)`——
**这只检查壳**，壳在页面初始化时就存在，`search.feeds` 要等接口回来才填。慢网络下软超时放行、
壳检查立即通过、提取拿到空值就直接报「没搜到结果」，比 panic 更难排查。所以 `wait.go` 补了
第三档 `softWaitData`，等的是业务数据本身而不是页面外观。

> **规矩：软超时和强就绪条件必须成对出现。** 只放宽等待、不加强判据，等于把崩溃换成了
> 静默的错误答案。

云端容器连续运行 3 天的对照：**59 次 panic → 0 次**。

### 2. 浏览器并发闸门（此前全项目零并发控制）

18 个 MCP 工具和 20 条 REST 端点共用一个 service 单例，每个业务方法独立 `newBrowser()`，
net/http 一请求一 goroutine——**两个请求撞上就是两套完整 Chromium**，一起把容器推向 OOM。
而 OOM 杀掉的是整个容器，不是那一个请求。

实测（空闲基线 237MB）：

| 并发 | 峰值内存 | 单请求耗时 | 吞吐 | 余量 |
|---|---|---|---|---|
| 1 | 528 MB | 9.3s | 6.5 次/分 | 672 MB |
| **2** | **736 MB** | **13s** | **9.2 次/分** | **464 MB** ← 取这个 |
| 3 | 975 MB | 18.5s | 9.7 次/分 | 225 MB |

取 N=2：吞吐已达峰值 95%，3 只多 5%（2 核已在争抢 CPU）却让延迟翻倍、余量掉到 225MB，
而评论多的详情页能轻松吃掉这点余量。`XHS_BROWSER_CONCURRENCY` 可覆盖，换机器要重新实测。

实现是零侵入的：19 个调用点在 browser 上只用了 `Close()` 和 `NewPage()`（已逐个核对），
于是让 `newBrowser()` 返回内嵌 `*headless_browser.Browser` 的 `leasedBrowser`——`NewPage`
靠 Go 的方法提升透传，只有 `Close` 被拦下来归还名额，**19 个调用点一处都不用改**。

> **量容器内存只能读 cgroup 的 `memory.current`**（与 `docker stats` 一致），不能把
> 多个 chrome 进程的 `ps` RSS 相加——后者会把进程间共享的二进制映射和 zygote 的 CoW 页
> 重复计算，实测高估 88%（996MB vs 528MB），照那个数字算会误判成「并发上限只能是 1」。

顺带修掉 `get_login_qrcode` 的浏览器泄漏：它是唯一一个浏览器要活过函数返回的方法，
所以 `deferFunc` 是拿到结果**之后**才按分支挂上的。可 `FetchQrcodeImage` 内部的 `Must*`
会 panic——那时 deferFunc 还没挂，整个浏览器进程组（约 300MB）永久泄漏，
在 1200MB 的容器里漏两次就够触发一次 OOM。改成默认关闭 + 显式移交。

以及 `feed_detail` 的 `retry.Attempts(3)` 一直是死代码：`retry.Do` 只认 error，
而里面写的是 `page.MustNavigate(url)`——失败是 **panic**，retry 根本捕获不到。
线上 8 次 `net::ERR_NAME_NOT_RESOLVED` 一次都没被重试过。改用返回 error 的
`page.Navigate(url)`，retry 才真正生效。

### 3. 识别安全验证，并把二维码递到账号本人手上

被小红书安全验证拦截时，拦截页也是一张正常的 HTML，读不到 `__INITIAL_STATE__` 就一路走到
「没搜到结果」——报出来的原因跟真实原因毫无关系，人只能靠猜。

- **识别**：判定只认 URL 里的 `/website-login/captcha` 这一段。`verifyUuid` 每次访问都不同、
  `redirectPath` 随来源页而变，而「安全验证」这类页面文案是站点随时能改的本地化字符串，
  只配写进错误消息，不配当触发条件。
- **取码**：新增第 19 个 MCP 工具 `get_verification_qrcode`，把服务端 headless 会话里的
  那张码原样搬出来给账号本人扫。
- **一次性验证链接**：二维码透进对话里，只有人坐在电脑前才好使。再发一条手机浏览器能直接打开的
  网页——一张大二维码、过期自动换新、验证一通过就显示结果。

网页只做搬运：码是小红书发的，扫的是账号本人的 App，**不解码、不识别、不自动提交**，
不含任何绕过验证的手段。

`/verify/*` 这条路由故意放在 Bearer 鉴权之外（手机浏览器发不了 `Authorization` 头），
所以路径里那个 32 字节随机 token 就是唯一凭证。相应地：15 分钟 TTL、常数时间比较、
**不写进访问日志**（gin 的 Logger 会把完整路径落盘，照写等于把凭证随日志采集流到别处）、
页面不引任何外部资源。公网地址服务端猜不到，用 `XHS_PUBLIC_BASE_URL` 告诉它。

### 4. 视频笔记返回字幕正文

视频画面大模型读不了，但字幕能读——它就是视频内容的文字转录。字幕 URL 本来就在
`__INITIAL_STATE__` 里，只是正文要另外下载 `.srt`，且链接带签名有时效，客户端拿到也取不动。
改成服务端下载好、去掉时间轴、直接把台词交出去（`VideoDetail.SubtitleText`）。

实测：一条 166 秒的视频 → 47 条字幕 / 1118 字完整中文转录，下载耗时 0.23s。

### 5. 读取类流程拦掉图片 / 音视频 / 字体请求

数据都在 `__INITIAL_STATE__` 里，封面图不必真的下载解码。

需要说明的是**收益有限**：原以为「视频播放是 DOM 抖动的唯一来源」，实测证伪——拦截后 DOM
仍不稳定，小红书视频很可能走 MSE，分片请求类型是 XHR 而非 Media，按类型拦不到。
保留是因为确实省了内存，但别指望它解决并发问题。

### 6. 时间戳给成人能读的格式

笔记、评论、通知此前只给裸的毫秒时间戳，模型要自己换算，经常算错年份。补上可读时间。

### 效果汇总

同一条视频笔记、同一个关键词，补丁全部部署后的对照：

| 调用 | 上游 | 本仓库 | |
|---|---|---|---|
| `get_feed_detail`（视频） | **>115s 未返回，最坏 10 分钟** | **10.2s** | −91% |
| `get_feed_detail`（图文） | 13.9s | 7.9s | −43% |
| `search_feeds` | 11.1s | 9.1s | −18% |
| 视频字幕 | 无 | **1118 字可读文本** | 新增 |
| 3 天内 panic 次数 | 59 | **0** | |

并发闸门实测（同时发 4 个请求）：全部成功，耗时分成 14.4/15.5s 与 26.5/26.6s 两批，
峰值 777MB，0 panic、无 OOM。

### 还没做的

**浏览器复用**（能省掉每次约 5 秒的冷启动）需要先处理两件事：`rod.Page.Close()` 持
Browser 级全局锁 `targetsLock`（可改用 `proto.TargetCloseTarget` 绕开），以及登录流程会
`Close()` 掉共享浏览器（要改成只关 page）。指纹方面没有障碍：seed 是启动 flag 且已全局钉死，
复用与否指纹一致。

## 安装

**注意**：`go.mod` 的模块路径保持上游的 `github.com/xpzouying/xiaohongshu-mcp` 未改
（改了会让每一个文件都产生 diff，反而看不清改了什么），所以 `go install` 装不了本仓库，
只能 clone 后构建：

```bash
git clone https://github.com/CNQQC/xhs-mcp.git
cd xhs-mcp
go build -o xhs-mcp .
go build -o login ./cmd/login
```

其余安装方式、登录、配置、MCP 客户端接入的步骤与上游完全一致，见下文。上游的
[Docker 镜像](https://hub.docker.com/r/xpzouying/xiaohongshu-mcp)不含本仓库的改动，
要用容器请自行 `docker build`。

---

以下文档承自上游，仅修正了与本仓库有出入的地方。

## 项目简介

**主要功能**

> 💡 **提示：** 点击下方功能标题可展开查看视频演示

<details>
<summary><b>1. 登录和检查登录状态</b></summary>

第一步必须，小红书需要进行登录。可以检查当前登录状态。

**登录演示：**

https://github.com/user-attachments/assets/8b05eb42-d437-41b7-9235-e2143f19e8b7

**检查登录状态演示：**

https://github.com/user-attachments/assets/bd9a9a4a-58cb-4421-b8f3-015f703ce1f9

</details>

<details>
<summary><b>2. 发布图文内容</b></summary>

支持发布图文内容到小红书，包括标题、内容描述和图片。

**图片支持方式：**

支持两种图片输入方式：

1. **HTTP/HTTPS 图片链接**

   ```
   ["https://example.com/image1.jpg", "https://example.com/image2.png"]
   ```

2. **本地图片绝对路径**（推荐）
   ```
   ["/Users/username/Pictures/image1.jpg", "/home/user/images/image2.png"]
   ```

**为什么推荐使用本地路径：**

- ✅ 稳定性更好，不依赖网络
- ✅ 上传速度更快
- ✅ 避免图片链接失效问题
- ✅ 支持更多图片格式

**发布图文帖子演示：**

https://github.com/user-attachments/assets/8aee0814-eb96-40af-b871-e66e6bbb6b06

</details>

<details>
<summary><b>3. 发布视频内容</b></summary>

支持发布视频内容到小红书，包括标题、内容描述和本地视频文件。

**视频支持方式：**

仅支持本地视频文件绝对路径：

```
"/Users/username/Videos/video.mp4"
```

**功能特点：**

- ✅ 支持本地视频文件上传
- ✅ 自动处理视频格式转换
- ✅ 支持标题、内容描述和标签
- ✅ 等待视频处理完成后自动发布

**注意事项：**

- 仅支持本地视频文件，不支持 HTTP 链接
- 视频处理时间较长，请耐心等待
- 建议视频文件大小不超过 1GB

</details>

<details>
<summary><b>4. 搜索内容</b></summary>

根据关键词搜索小红书内容。

**搜索帖子演示：**

https://github.com/user-attachments/assets/03c5077d-6160-4b18-b629-2e40933a1fd3

</details>

<details>
<summary><b>5. 获取推荐列表</b></summary>

获取小红书首页推荐内容列表。

**获取推荐列表演示：**

https://github.com/user-attachments/assets/110fc15d-46f2-4cca-bdad-9de5b5b8cc28

</details>

<details>
<summary><b>6. 获取帖子详情（包括互动数据和评论）</b></summary>

获取小红书帖子的完整详情，包括：

- 帖子内容（标题、描述、图片等）
- 用户信息
- 互动数据（点赞、收藏、分享、评论数）
- 评论列表及子评论

**⚠️ 重要提示：**

- 需要提供帖子 ID 和 xsec_token（两个参数缺一不可）
- 这两个参数可以从 Feed 列表或搜索结果中获取
- 必须先登录才能使用此功能

**获取帖子详情演示：**

https://github.com/user-attachments/assets/76a26130-a216-4371-a6b3-937b8fda092a

</details>

<details>
<summary><b>7. 发表评论到帖子</b></summary>

支持自动发表评论到小红书帖子。

**功能说明：**

- 自动定位评论输入框
- 输入评论内容并发布
- 支持 HTTP API 和 MCP 工具调用

**⚠️ 重要提示：**

- 需要先登录才能使用此功能
- 需要提供帖子 ID、xsec_token 和评论内容
- 这些参数可以从 Feed 列表或搜索结果中获取

**发表评论演示：**

https://github.com/user-attachments/assets/cc385b6c-422c-489b-a5fc-63e92c695b80

</details>

<details>
<summary><b>8. 获取用户个人主页</b></summary>

获取小红书用户的个人主页信息，包括用户基本信息和笔记内容。

**功能说明：**

- 获取用户基本信息（昵称、简介、头像等）
- 获取关注数、粉丝数、获赞量统计
- 获取用户发布的笔记内容列表
- 支持 HTTP API 和 MCP 工具调用

**⚠️ 重要提示：**

- 需要先登录才能使用此功能
- 需要提供用户 ID 和 xsec_token
- 这些参数可以从 Feed 列表或搜索结果中获取

**返回信息包括：**

- 用户基本信息：昵称、简介、头像、认证状态
- 统计数据：关注数、粉丝数、获赞量、笔记数
- 笔记列表：用户发布的所有公开笔记

</details>

<details>
<summary><b>9. 回复评论</b></summary>

回复笔记下的指定评论，支持精准回复特定用户的评论。

**功能说明：**

- 回复指定笔记下的特定评论
- 支持通过评论 ID 或用户 ID 定位目标评论
- 需要提供 feed_id、xsec_token、comment_id/user_id 和回复内容

**⚠️ 重要提示：**

- 需要先登录才能使用此功能
- comment_id 和 user_id 至少提供一个
- 这些参数可以从帖子详情的评论列表中获取

</details>

<details>
<summary><b>10. 点赞/取消点赞</b></summary>

为笔记点赞或取消点赞，智能检测当前状态避免重复操作。

**功能说明：**

- 为指定笔记点赞或取消点赞
- 智能检测：已点赞时跳过点赞，未点赞时跳过取消点赞
- 需要提供 feed_id 和 xsec_token

**⚠️ 重要提示：**

- 需要先登录才能使用此功能
- 默认为点赞操作，设置 unlike=true 可取消点赞

</details>

<details>
<summary><b>11. 收藏/取消收藏</b></summary>

收藏笔记或取消收藏，智能检测当前状态避免重复操作。

**功能说明：**

- 收藏指定笔记或取消收藏
- 智能检测：已收藏时跳过收藏，未收藏时跳过取消收藏
- 需要提供 feed_id 和 xsec_token

**⚠️ 重要提示：**

- 需要先登录才能使用此功能
- 默认为收藏操作，设置 unfavorite=true 可取消收藏

</details>

**小红书基础运营知识**

- **标题：（非常重要）小红书要求标题不超过 20 个字**
- **正文：（非常重要）：正文不能超过 1000 个字**
- 当前支持图文发送以及视频发送：从推荐的角度看，图文的流量会比视频以及纯文字的更好。
- （低优先级）可以考虑纯文字的支持。1. 个人感觉纯文字会大大增加运营的复杂度；2. 纯文字在我的使用场景的价值较低。
- Tags：现已支持。添加合适的 Tags 能带来更多的流量。
- 根据本人实操，小红书每天的发帖量应该是 **50 篇**。
- **（非常重要）小红书的同一个账号不允许在多个网页端登录**，如果你登录了当前 xiaohongshu-mcp 后，就不要再在其他的网页端登录该账号，否则就会把当前 MCP 的账号“踢出登录”。你可以使用移动 App 端进行查看当前账号信息。
- 曝光低的话，首先查看内容中是否有违禁词，搜一下有很多第三方免费工具。
- 一定不要出现引流、纯搬运的情况，属于官方重点打击对象。

**风险说明**

1. 该项目是在自己的另外一个项目的基础上开源出来的，原来的项目稳定运行一年多，没有出现过封号的情况，只有出现过 Cookies 过期需要重新登录。
2. 我是使用 Claude Code 接入，稳定自动化运营数周后，验证没有问题后开源。
3. 如果账号没有实名认证，特别是新号，一般会触发 **实名认证** 的消息提醒（参见下图）。⚠️ 这个不是封号，不用 MCP 也会要求实名认证。实名认证后，账号就正常了。建议使用该项目前就先实名。
   <img width="508" height="306" alt="image" src="https://github.com/user-attachments/assets/34383e1b-f666-409f-9870-002655507dc1" />

该项目是基于学习的目的，禁止一切违法行为。

**实操结果**

第一天点赞/收藏数达到了 999+，

<img width="386" height="278" alt="CleanShot 2025-09-05 at 01 31 55@2x" src="https://github.com/user-attachments/assets/4b5a283b-bd38-45b8-b608-8f818997366c" />

<img width="350" height="280" alt="CleanShot 2025-09-05 at 01 32 49@2x" src="https://github.com/user-attachments/assets/4481e1e7-3ef6-4bbd-8483-dcee8f77a8f2" />

一周左右的成果

<img width="1840" height="582" alt="CleanShot 2025-09-05 at 01 33 13@2x" src="https://github.com/user-attachments/assets/fb367944-dc48-4bbd-8ece-934caa86323e" />

## 1. 使用教程

### 1.1. 快速开始（推荐）

**方式一：下载预编译二进制文件**

> [!WARNING]
> **本仓库暂未提供 Releases 二进制。** 下面这个链接是**上游**的 Releases，
> 那里的二进制**不含本仓库的任何改动**——它会带着上游那些超时和 OOM 问题。
> 想要本仓库的版本，请用[上面的构建步骤](#安装)自行编译。
>
> 平台清单和使用步骤上下游一致，保留在这里供参考。

直接从[上游 GitHub Releases](https://github.com/xpzouying/xiaohongshu-mcp/releases) 下载对应平台的二进制文件：

**主程序（MCP 服务）：**

- **macOS Apple Silicon**: `xiaohongshu-mcp-darwin-arm64`
- **Windows x64**: `xiaohongshu-mcp-windows-amd64.exe`
- **Linux x64**: `xiaohongshu-mcp-linux-amd64`

**登录工具：**

- **macOS Apple Silicon**: `xiaohongshu-login-darwin-arm64`
- **Windows x64**: `xiaohongshu-login-windows-amd64.exe`
- **Linux x64**: `xiaohongshu-login-linux-amd64`

> 目前支持以上三个平台。macOS Intel 与 Linux ARM64 暂不支持。

使用步骤：

```bash
# 1. 首先运行登录工具
chmod +x xiaohongshu-login-darwin-arm64
./xiaohongshu-login-darwin-arm64

# 2. 然后启动 MCP 服务
chmod +x xiaohongshu-mcp-darwin-arm64
./xiaohongshu-mcp-darwin-arm64
```

**⚠️ 重要提示**：首次运行时会自动下载无头浏览器（约 150MB），请确保网络连接正常。后续运行无需重复下载。

**方式二：源码编译**

<details>
<summary>源码编译安装详情</summary>

依赖 Golang 环境，安装方法请参考 [Golang 官方文档](https://go.dev/doc/install)。

设置 Go 国内源的代理，

```bash
# 配置 GOPROXY 环境变量，以下三选一

# 1. 七牛 CDN
go env -w  GOPROXY=https://goproxy.cn,direct

# 2. 阿里云
go env -w GOPROXY=https://mirrors.aliyun.com/goproxy/,direct

# 3. 官方
go env -w  GOPROXY=https://goproxy.io,direct
```

</details>

**方式三：使用 Docker 容器（最简单）**

<details>
<summary>Docker 部署详情</summary>

使用 Docker 部署是最简单的方式，无需安装任何开发环境。

**1. 从 Docker Hub 拉取镜像**

> [!WARNING]
> **这是上游的镜像，不含本仓库的任何改动。** 本仓库没有发布自己的镜像，
> 要在容器里跑本仓库的版本，请用下面第 3 条自行构建。

```bash
# 拉取上游最新镜像（不含本仓库改动）
docker pull xpzouying/xiaohongshu-mcp
```

Docker Hub 地址：[https://hub.docker.com/r/xpzouying/xiaohongshu-mcp](https://hub.docker.com/r/xpzouying/xiaohongshu-mcp)

**2. 使用 Docker Compose 启动（推荐）**

我们提供了配置好的 `docker-compose.yml` 文件，可以直接使用：

```bash
# 下载 docker-compose.yml
wget https://raw.githubusercontent.com/CNQQC/xhs-mcp/main/docker/docker-compose.yml

# 或者如果已经克隆了项目，进入 docker 目录
cd docker

# 启动服务
docker compose up -d

# 查看日志
docker compose logs -f

# 停止服务
docker compose stop
```

**3. 自己构建镜像（要用本仓库的改动，走这条）**

```bash
# 在项目根目录运行
docker build -t xhs-mcp .
```

构建出来的镜像名是 `xhs-mcp`，`docker-compose.yml` 里的 `image:` 也要相应改掉。

**4. 配置说明**

Docker 版本会自动：

- 配置内置浏览器和中文字体
- 挂载 `./data` 用于存储 cookies 和运行数据目录
- 挂载 `./images` 用于存储发布的图片
- 暴露 18060 端口供 MCP 连接

详细使用说明请参考：[Docker 部署指南](./docker/README.md)

</details>

Windows 遇到问题首先看这里：[Windows 安装指南](./docs/windows_guide.md)

### 1.2. 登录

第一次需要手动登录，需要保存小红书的登录状态。

**使用二进制文件**：

```bash
# 运行对应平台的登录工具
./xiaohongshu-login-darwin-arm64
```

**使用源码**：

```bash
go run cmd/login/main.go
```

### 1.3. 启动 MCP 服务

启动 xiaohongshu-mcp 服务。

**使用二进制文件**：

```bash
# 默认：无头模式，没有浏览器界面
./xiaohongshu-mcp-darwin-arm64

# 非无头模式，有浏览器界面
./xiaohongshu-mcp-darwin-arm64 -headless=false
```

**使用源码**：

```bash
# 默认：无头模式，没有浏览器界面
go run .

# 非无头模式，有浏览器界面
go run . -headless=false
```

**配置代理（可选）**：

如果需要通过代理访问，可以设置 `XHS_PROXY` 环境变量：

```bash
# 设置代理后启动
XHS_PROXY=http://user:pass@proxy:port ./xiaohongshu-mcp-darwin-arm64

# 或使用源码
XHS_PROXY=http://proxy:port go run .
```

支持 HTTP/HTTPS/SOCKS5 代理，日志中会自动隐藏代理的认证信息。

**验证链接的公网地址（可选）**：

被小红书安全验证拦截时，`get_verification_qrcode` 除了把二维码透进对话里，还会发一条一次性验证网页链接——手机浏览器打开就是一张大二维码，过期自动换新，验证一通过就显示结果，适合人不在电脑前的时候。

服务端猜不到自己的公网地址，需要用 `XHS_PUBLIC_BASE_URL` 告诉它，否则只会给出相对路径 `/verify/<token>`：

```bash
XHS_PUBLIC_BASE_URL=https://api.example.com/xhs ./xhs-mcp
```

`/verify/*` 这两条路由**不走 `AUTH_TOKEN` 鉴权**（手机浏览器发不了 `Authorization` 头），路径里那个 32 字节随机 token 就是凭证：15 分钟有效、不写进访问日志、页面不引任何外部资源。网页只是把小红书发来的验证码原样显示出来，由账号本人扫码，不做任何自动验证。

**浏览器并发上限（可选，本仓库新增）**：

每个请求都会起一整个 Chromium 进程组。默认最多同时开 **2 个**，超出的请求排队，
排不到就在 60 秒后明确失败，不做长排队。

```bash
XHS_BROWSER_CONCURRENCY=2 ./xhs-mcp
```

默认值 2 是在 **2 核 / 容器内存上限 1200MB** 上实测出来的（详见上文[相对上游的改动](#2-浏览器并发闸门此前全项目零并发控制)）。
**换机器一定要重新实测**——尤其是改过 `mem_limit` 或 CPU 数之后。调大到容器扛不住的程度，
OOM 杀掉的是整个容器而不是那一个请求。

**访问鉴权（可选）**：

默认关闭鉴权。生产环境建议使用 `AUTH_TOKEN` 环境变量配置；非空的启动参数优先于环境变量，留空则读取 `AUTH_TOKEN`。

```bash
# 环境变量
AUTH_TOKEN=your-secret-token ./xiaohongshu-mcp-darwin-arm64
AUTH_TOKEN=your-secret-token go run .

# 非空启动参数（优先于环境变量）
./xiaohongshu-mcp-darwin-arm64 -token=your-secret-token
go run . -token=your-secret-token
```

启用鉴权后，所有 MCP 客户端都必须配置自定义请求头 `Authorization: Bearer <token>`。命令行参数可能被进程列表看到，部署环境优先使用 `AUTH_TOKEN`。

例如，支持自定义请求头的 MCP 客户端可使用以下配置：

```json
{
  "mcpServers": {
    "xiaohongshu-mcp": {
      "url": "http://localhost:18060/mcp",
      "headers": { "Authorization": "Bearer your-secret-token" }
    }
  }
}
```

## 1.4. 验证 MCP

```bash
npx @modelcontextprotocol/inspector
```

![运行 Inspector](./assets/run_inspect.png)

运行后，打开红色标记的链接，配置 MCP inspector，输入 `http://localhost:18060/mcp` ，点击 `Connect` 按钮。

<img width="915" height="659" alt="bf9532dd0b7ba423491accf511a467de" src="https://github.com/user-attachments/assets/08bc3cef-73e7-42d2-b923-7ba9e6c8af30" />

**注意：** 左侧边框中的选项是否正确。

按照上面配置 MCP inspector 后，点击 `List Tools` 按钮，查看所有的 Tools。

## 1.5. 使用 MCP 发布

### 检查登录状态

![检查登录状态](./assets/check_login.gif)

### 发布图文

示例中是从 https://unsplash.com/ 中随机找了个图片做测试。

![发布图文](./assets/inspect_mcp_publish.gif)

### 搜索内容

使用搜索功能，根据关键词搜索小红书内容：

![搜索内容](./assets/search_result.png)

## 2. MCP 客户端接入

本服务支持标准的 Model Context Protocol (MCP)，可以接入各种支持 MCP 的 AI 客户端。

### 2.1. 快速开始

#### 启动 MCP 服务

```bash
# 启动服务（默认无头模式）
go run .

# 或者有界面模式
go run . -headless=false
```

服务将运行在：`http://localhost:18060/mcp`

#### 验证服务状态

```bash
# 测试 MCP 连接
curl -X POST http://localhost:18060/mcp \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-secret-token" \
  -d '{"jsonrpc":"2.0","method":"initialize","params":{},"id":1}'
```

#### Claude Code CLI 接入

```bash
# 添加 HTTP MCP 服务器
claude mcp add --transport http xiaohongshu-mcp http://localhost:18060/mcp

# 检查 MCP 是否添加成功（确保 MCP 已经启动的前提下，运行下面命令）
claude mcp list
```

### 2.2. 支持的客户端

<details>
<summary><b>Claude Code CLI</b></summary>

官方命令行工具，已在上面快速开始部分展示：

```bash
# 添加 HTTP MCP 服务器
claude mcp add --transport http xiaohongshu-mcp http://localhost:18060/mcp

# 检查 MCP 是否添加成功（确保 MCP 已经启动的前提下，运行下面命令）
claude mcp list
```

</details>

<details>
<summary><b>Open Code CLI</b></summary>

使用交互式命令添加 MCP Server：

```bash
opencode mcp add
```

以添加 `xiaohongshu-mcp` 为例：

```
┌  Add MCP server
│
◇  Enter MCP server name
│  xiaohongshu-mcp
│
◇  Select MCP server type
│  Remote
│
◇  Enter MCP server URL
│  http://localhost:18060/mcp
│
◇  Does this server require OAuth authentication?
│  No
│
◆  MCP server "xiaohongshu-mcp" added to C:\Users\admin\.config\opencode\opencode.json
│
└  MCP server added successfully
```

验证是否添加成功（确保 MCP 已启动的前提下）：

```bash
opencode mcp list
```

```
┌  MCP Servers
│
●  ✓ xiaohongshu-mcp connected
```

</details>

<details>
<summary><b>Cursor</b></summary>

#### 配置文件的方式

创建或编辑 MCP 配置文件：

**项目级配置**（推荐）：
在项目根目录创建 `.cursor/mcp.json`：

```json
{
  "mcpServers": {
    "xiaohongshu-mcp": {
      "url": "http://localhost:18060/mcp",
      "description": "小红书内容发布服务 - MCP Streamable HTTP"
    }
  }
}
```

**全局配置**：
在用户目录创建 `~/.cursor/mcp.json` (同样内容)。

#### 使用步骤

1. 确保小红书 MCP 服务正在运行
2. 保存配置文件后，重启 Cursor
3. 在 Cursor 聊天中，工具应该自动可用
4. 可以通过聊天界面的 "Available Tools" 查看已连接的 MCP 工具

**Demo**

插件 MCP 接入：

![cursor_mcp_settings](./assets/cursor_mcp_settings.png)

调用 MCP 工具：（以检查登录状态为例）

![cursor_mcp_check_login](./assets/cursor_mcp_check_login.png)

</details>

<details>
<summary><b>VSCode</b></summary>

#### 方法一：使用命令面板配置

1. 按 `Ctrl/Cmd + Shift + P` 打开命令面板
2. 运行 `MCP: Add Server` 命令
3. 选择 `HTTP` 方式。
4. 输入地址： `http://localhost:18060/mcp`，或者修改成对应的 Server 地址。
5. 输入 MCP 名字： `xiaohongshu-mcp`。

#### 方法二：直接编辑配置文件

**工作区配置**（推荐）：
在项目根目录创建 `.vscode/mcp.json`：

```json
{
  "servers": {
    "xiaohongshu-mcp": {
      "url": "http://localhost:18060/mcp",
      "type": "http"
    }
  },
  "inputs": []
}
```

**查看配置**：

![vscode_config](./assets/vscode_mcp_config.png)

1. 确认运行状态。
2. 查看 `tools` 是否正确检测。

**Demo**

以搜索帖子内容为例：

![vscode_mcp_search](./assets/vscode_search_demo.png)

</details>

<details>
<summary><b>Google Gemini CLI</b></summary>

在 `~/.gemini/settings.json` 或项目目录 `.gemini/settings.json` 中配置：

```json
{
  "mcpServers": {
    "xiaohongshu": {
      "httpUrl": "http://localhost:18060/mcp",
      "timeout": 30000
    }
  }
}
```

更多信息请参考 [Gemini CLI MCP 文档](https://google-gemini.github.io/gemini-cli/docs/tools/mcp-server.html)

</details>

<details>
<summary><b>MCP Inspector</b></summary>

调试工具，用于测试 MCP 连接：

```bash
# 启动 MCP Inspector
npx @modelcontextprotocol/inspector

# 在浏览器中连接到：http://localhost:18060/mcp
```

使用步骤：

- 使用 MCP Inspector 测试连接
- 测试 Ping Server 功能验证连接
- 检查 List Tools 是否返回 13 个工具

</details>

<details>
<summary><b>Cline</b></summary>

Cline 是一个强大的 AI 编程助手，支持 MCP 协议集成。

#### 配置方法

在 Cline 的 MCP 设置中添加以下配置：

```json
{
  "xiaohongshu-mcp": {
    "url": "http://localhost:18060/mcp",
    "type": "streamableHttp",
    "autoApprove": [],
    "disabled": false
  }
}
```

#### 使用步骤

1. 确保小红书 MCP 服务正在运行（`http://localhost:18060/mcp`）
2. 在 Cline 中打开 MCP 设置
3. 添加上述配置到 MCP 服务器列表
4. 保存配置并重启 Cline
5. 在对话中可以直接使用小红书相关功能

#### 配置说明

- `url`: MCP 服务地址
- `type`: 使用 `streamableHttp` 类型以获得更好的性能
- `autoApprove`: 可配置自动批准的工具列表（留空表示手动批准）
- `disabled`: 设置为 `false` 启用此 MCP 服务

#### 使用示例

配置完成后，可以在 Cline 中直接使用自然语言操作小红书：

```
帮我检查小红书登录状态
```

```
帮我发布一篇关于春天的图文到小红书，使用这张图片：/path/to/spring.jpg
```

```
搜索小红书上关于"美食"的内容
```

</details>
<details>
<summary><b>OpenClaw（通过 MCPorter）</b></summary>

> 使用前请确保 xiaohongshu-mcp 已完成本地部署。**不建议**将 GitHub 链接直接丢给 OpenClaw 让其代为部署。

由于 OpenClaw 目前不原生支持 MCP，官方推荐通过 **MCPorter** 来调用 MCP 服务。

> 💡 **提示：** MCPorter 并非调用 MCP 的最佳方案，使用过程中可能出现一些兼容性问题，请知悉。

#### 安装与配置步骤

直接一次性将一下三行命令丢给 OpenClaw（可以是 Control UI、Telegram、Feishu等方式），Openclaw 会代为部署 MCPorter。

```
npm i -g mcporter
npx mcporter config add xiaohongshu-mcp http://localhost:18060/mcp
npx mcporter list xiaohongshu-mcp
```

完成上述步骤后，即可在 OpenClaw 中通过自然语言调用 xiaohongshu-mcp 的所有功能。

</details>
<details>
<summary><b>其他支持 HTTP MCP 的客户端</b></summary>

任何支持 HTTP MCP 协议的客户端都可以连接到：`http://localhost:18060/mcp`

基本配置模板：

```json
{
  "name": "xiaohongshu-mcp",
  "url": "http://localhost:18060/mcp",
  "type": "http"
}
```

</details>

### 2.3. 可用 MCP 工具

连接成功后，可使用以下 MCP 工具：

- `check_login_status` - 检查小红书登录状态（无参数）
- `get_login_qrcode` - 获取登录二维码，返回 Base64 图片和超时时间（无参数）
- `delete_cookies` - 删除 cookies 文件，重置登录状态，删除后需要重新登录（无参数）
- `publish_content` - 发布图文内容到小红书（必需：title, content, images）
  - `images`: 图片路径列表（至少1张），支持 HTTP 链接或本地绝对路径，推荐使用本地路径
  - `tags`: 话题标签列表（可选），如 `["美食", "旅行", "生活"]`
  - `schedule_at`: 定时发布时间（可选），ISO8601 格式，支持 1 小时至 14 天内
  - `is_original`: 是否声明原创（可选），默认不声明
  - `visibility`: 可见范围（可选），支持 `公开可见`（默认）、`仅自己可见`、`仅互关好友可见`
  - `products`: 商品关键词列表（可选），用于绑定带货商品。填写商品名称或商品ID，系统会自动搜索并选择第一个匹配结果。需账号已开通商品功能。示例: [面膜, 防晒霜SPF50]
- `publish_with_video` - 发布视频内容到小红书（必需：title, content, video）
  - `video`: 本地视频文件绝对路径（仅支持单个视频文件）
  - `tags`: 话题标签列表（可选），如 `["美食", "旅行", "生活"]`
  - `schedule_at`: 定时发布时间（可选），ISO8601 格式，支持 1 小时至 14 天内
  - `visibility`: 可见范围（可选），支持 `公开可见`（默认）、`仅自己可见`、`仅互关好友可见`
  - `products`: 商品关键词列表（可选），用于绑定带货商品。填写商品名称或商品ID，系统会自动搜索并选择第一个匹配结果。需账号已开通商品功能。示例: [面膜, 防晒霜SPF50]
- `list_feeds` - 获取小红书首页推荐列表（无参数）
- `search_feeds` - 搜索小红书内容（必需：keyword）
  - `filters`: 筛选选项（可选）
    - `sort_by`: 排序依据 - `综合`（默认）| `最新` | `最多点赞` | `最多评论` | `最多收藏`
    - `note_type`: 笔记类型 - `不限`（默认）| `视频` | `图文`
    - `publish_time`: 发布时间 - `不限`（默认）| `一天内` | `一周内` | `半年内`
    - `search_scope`: 搜索范围 - `不限`（默认）| `已看过` | `未看过` | `已关注`
    - `location`: 位置距离 - `不限`（默认）| `同城` | `附近`
- `get_feed_detail` - 获取帖子详情，包括互动数据和评论（必需：feed_id, xsec_token）
  - `load_all_comments`: 是否加载全部评论（可选），默认 false 仅返回前 10 条一级评论
  - `limit`: 限制加载的一级评论数量（可选），仅当 load_all_comments=true 时生效，默认 20
  - `click_more_replies`: 是否展开二级回复（可选），仅当 load_all_comments=true 时生效，默认 false
  - `reply_limit`: 跳过回复数过多的评论（可选），仅当 click_more_replies=true 时生效，默认 10
  - `scroll_speed`: 滚动速度（可选），`slow` | `normal` | `fast`，仅当 load_all_comments=true 时生效
  - 🆕 **本仓库新增**：视频笔记会额外返回 `video.subtitleText`——服务端下载好字幕、
    去掉时间轴后的完整台词文本。视频画面模型读不了，字幕能读。
  - 🆕 **本仓库新增**：笔记与评论的时间戳同时给出可读格式，模型不用自己换算
- `post_comment_to_feed` - 发表评论到小红书帖子（必需：feed_id, xsec_token, content）
- `reply_comment_in_feed` - 回复笔记下的指定评论（必需：feed_id, xsec_token, content，以及 comment_id 或 user_id 至少一个）
- `like_feed` - 点赞/取消点赞（必需：feed_id, xsec_token）
  - `unlike`: 是否取消点赞（可选），true 为取消点赞，默认为点赞
- `favorite_feed` - 收藏/取消收藏（必需：feed_id, xsec_token）
  - `unfavorite`: 是否取消收藏（可选），true 为取消收藏，默认为收藏
- `user_profile` - 获取用户个人主页信息（必需：user_id, xsec_token）
- 🆕 `get_verification_qrcode` - **本仓库新增**。被小红书安全验证拦截时，取回服务端会话里
  的那张验证二维码交给账号本人扫（无参数）
  - 同时会给出一条一次性验证网页链接，手机浏览器直接打开就是一张大二维码、过期自动换新、
    验证通过即显示结果。要用这条链接得先用 `XHS_PUBLIC_BASE_URL` 告诉服务端自己的公网地址，
    否则只返回相对路径
  - 只做搬运：码是小红书发的、扫的是账号本人的 App，**不解码、不识别、不自动提交**

> 🆕 **本仓库新增**：以上任何工具在被安全验证拦截时，都会明确返回「被小红书安全验证拦截」，
> 而不是像上游那样报成「没搜到结果」或直接超时——拿到这个错误就该去调 `get_verification_qrcode`，
> 重试没有意义。

### 2.4. 使用示例

使用 Claude Code 发布内容到小红书：

**示例 1：使用 HTTP 图片链接**

```
帮我写一篇帖子发布到小红书上，
配图为：https://cn.bing.com/th?id=OHR.MaoriRock_EN-US6499689741_UHD.jpg&w=3840
图片是："纽西兰陶波湖的Ngātoroirangi矿湾毛利岩雕（© Joppi/Getty Images）"

使用 xiaohongshu-mcp 进行发布。
```

**示例 2：使用本地图片路径（推荐）**

```
帮我写一篇关于春天的帖子发布到小红书上，
使用这些本地图片：
- /Users/username/Pictures/spring_flowers.jpg
- /Users/username/Pictures/cherry_blossom.jpg

使用 xiaohongshu-mcp 进行发布。
```

**示例 3：发布视频内容**

```
帮我写一篇关于美食制作的视频发布到小红书上，
使用这个本地视频文件：
- /Users/username/Videos/cooking_tutorial.mp4

使用 xiaohongshu-mcp 的视频发布功能。
```

![claude-cli 进行发布](./assets/claude_push.gif)

**发布结果：**

<img src="./assets/publish_result.jpeg" alt="xiaohongshu-mcp 发布结果" width="300">

### 2.5. 💬 MCP 使用常见问题解答

---

> ⚠️ 以下是使用 OpenClaw + MCPorter 时的已知风险，使用前请充分了解：

- OpenClaw 的 AI 自动部署行为不在本项目的维护范围内，部署结果无法保证
- MCPorter 作为中间层可能引入额外的兼容性问题，与 xiaohongshu-mcp 本身无关
- 若遇到连接失败、工具调用异常等问题，请先排查 MCPorter 自身的配置，而非提交 Issue
- 在提问社区或群组前，请先确认问题是否能在**不使用 OpenClaw** 的情况下复现

如果你没有强烈的 OpenClaw 使用需求，强烈建议改用 [Claude Code CLI](#claude-code-cli)、[Cursor](#cursor) 或 [Cline](#cline) 等原生支持 HTTP MCP 的客户端，体验会更稳定。

---

**Q:** 为什么检查登录用户名显示 `xiaghgngshu-mcp`？
**A:** 用户名是写死的。

---

**Q:** 显示发布成功后，但实际上没有显示？
**A:** 排查步骤如下：

1. 使用 **非无头模式** 重新发布一次。
2. 更换 **不同的内容** 重新发布。
3. 登录网页版小红书，查看账号是否被 **风控限制网页版发布**。
4. 检查 **图片大小** 是否过大。
5. 确认 **图片路径中没有中文字符**。
6. 若使用网络图片地址，请确认 **图片链接可正常访问**。

---

**Q:** 在设备上运行 MCP 程序出现闪退如何解决？
**A:**

1. 建议 **从源码安装**。
2. 或使用 **Docker 安装 xiaohongshu-mcp**，教程参考：
   - [使用 Docker 安装 xiaohongshu-mcp](https://github.com/xpzouying/xiaohongshu-mcp#:~:text=%E6%96%B9%E5%BC%8F%E4%B8%89%EF%BC%9A%E4%BD%BF%E7%94%A8%20Docker%20%E5%AE%B9%E5%99%A8%EF%BC%88%E6%9C%80%E7%AE%80%E5%8D%95%EF%BC%89)
   - [X-MCP 项目页面](https://github.com/xpzouying/x-mcp/)

---

**Q:** 使用 `http://localhost:18060/mcp` 进行 MCP 验证时提示无法连接？
**A:**

- 在 **Docker 环境** 下，请使用
  👉 [http://host.docker.internal:18060/mcp](http://host.docker.internal:18060/mcp)
- 在 **非 Docker 环境** 下，请使用 **本机 IPv4 地址** 访问。

---

## 3. 🌟 实战案例展示 (Community Showcases)

> 💡 **强烈推荐查看**：这些都是社区贡献者的真实使用案例，包含详细的配置步骤和实战经验！

### 📚 完整教程列表

1. **[n8n 完整集成教程](./examples/n8n/README.md)** - 工作流自动化平台集成
2. **[Cherry Studio 完整配置教程](./examples/cherrystudio/README.md)** - AI 客户端完美接入
3. **[Claude Code + Kimi K2 接入教程](./examples/claude-code/claude-code-kimi-k2.md)** - Claude Code 门槛太高，那么就接入 Kimi 国产大模型吧～
4. **[AnythingLLM 完整指南](./examples/anythingLLM/readme.md)** - AnythingLLM 是一款 all-in-one 多模态 AI 客户端，支持 workflow 定义，支持多种大模型和插件扩展。

> 🎯 **提示**: 点击上方链接查看详细的图文教程，快速上手各种集成方案！
>
> 📢 **欢迎贡献**: 如果你有新的集成案例，欢迎提交 PR 分享给社区！

## 4. 小红书 MCP 互助群

上游维护着微信群和飞书群，**入群二维码请到[上游 README](https://github.com/xpzouying/xiaohongshu-mcp#4-小红书-mcp-互助群)获取**——
二维码有时效，转贴过来只会过期误导人。

提问前请先看完文档和 [Issues](https://github.com/xpzouying/xiaohongshu-mcp/issues)。
另外请注意：**群是上游的，本仓库的改动不归群里负责**。属于本仓库的问题请提到
[本仓库的 Issues](https://github.com/CNQQC/xhs-mcp/issues)。

## 🙏 致谢贡献者 ✨

下面是**上游项目 [xpzouying/xiaohongshu-mcp](https://github.com/xpzouying/xiaohongshu-mcp)
的贡献者名单**。这个项目的绝大部分代码是他们写的，本仓库只是站在上面加了一层。
名单原样承自上游，链接均指向上游仓库。（排名不分先后）

<!-- ALL-CONTRIBUTORS-LIST:START - Do not remove or modify this section -->
<!-- prettier-ignore-start -->
<!-- markdownlint-disable -->
<table>
  <tbody>
    <tr>
      <td align="center" valign="top" width="14.28%"><a href="https://haha.ai"><img src="https://avatars.githubusercontent.com/u/3946563?v=4?s=100" width="100px;" alt="zy"/><br /><sub><b>zy</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=xpzouying" title="Code">💻</a> <a href="#ideas-xpzouying" title="Ideas, Planning, & Feedback">🤔</a> <a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=xpzouying" title="Documentation">📖</a> <a href="#design-xpzouying" title="Design">🎨</a> <a href="#maintenance-xpzouying" title="Maintenance">🚧</a> <a href="#infra-xpzouying" title="Infrastructure (Hosting, Build-Tools, etc)">🚇</a> <a href="https://github.com/xpzouying/xiaohongshu-mcp/pulls?q=is%3Apr+reviewed-by%3Axpzouying" title="Reviewed Pull Requests">👀</a></td>
      <td align="center" valign="top" width="14.28%"><a href="http://www.hwbuluo.com"><img src="https://avatars.githubusercontent.com/u/1271815?v=4?s=100" width="100px;" alt="clearwater"/><br /><sub><b>clearwater</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=esperyong" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/laryzhong"><img src="https://avatars.githubusercontent.com/u/47939471?v=4?s=100" width="100px;" alt="Zhongpeng"/><br /><sub><b>Zhongpeng</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=laryzhong" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/DTDucas"><img src="https://avatars.githubusercontent.com/u/105262836?v=4?s=100" width="100px;" alt="Duong Tran"/><br /><sub><b>Duong Tran</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=DTDucas" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/Angiin"><img src="https://avatars.githubusercontent.com/u/17389304?v=4?s=100" width="100px;" alt="Angiin"/><br /><sub><b>Angiin</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=Angiin" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/muhenan"><img src="https://avatars.githubusercontent.com/u/43441941?v=4?s=100" width="100px;" alt="Henan Mu"/><br /><sub><b>Henan Mu</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=muhenan" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/chengazhen"><img src="https://avatars.githubusercontent.com/u/52627267?v=4?s=100" width="100px;" alt="Journey"/><br /><sub><b>Journey</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=chengazhen" title="Code">💻</a></td>
    </tr>
    <tr>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/eveyuyi"><img src="https://avatars.githubusercontent.com/u/69026872?v=4?s=100" width="100px;" alt="Eve Yu"/><br /><sub><b>Eve Yu</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=eveyuyi" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/CooperGuo"><img src="https://avatars.githubusercontent.com/u/183056602?v=4?s=100" width="100px;" alt="CooperGuo"/><br /><sub><b>CooperGuo</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=CooperGuo" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://biboyqg.github.io/"><img src="https://avatars.githubusercontent.com/u/125724218?v=4?s=100" width="100px;" alt="Banghao Chi"/><br /><sub><b>Banghao Chi</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=BiboyQG" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/varz1"><img src="https://avatars.githubusercontent.com/u/60377372?v=4?s=100" width="100px;" alt="varz1"/><br /><sub><b>varz1</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=varz1" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://google.meloguan.site"><img src="https://avatars.githubusercontent.com/u/62586556?v=4?s=100" width="100px;" alt="Melo Y Guan"/><br /><sub><b>Melo Y Guan</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=Meloyg" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/lmxdawn"><img src="https://avatars.githubusercontent.com/u/21293193?v=4?s=100" width="100px;" alt="lmxdawn"/><br /><sub><b>lmxdawn</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=lmxdawn" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/haikow"><img src="https://avatars.githubusercontent.com/u/22428382?v=4?s=100" width="100px;" alt="haikow"/><br /><sub><b>haikow</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=haikow" title="Code">💻</a></td>
    </tr>
    <tr>
      <td align="center" valign="top" width="14.28%"><a href="https://carlo-blog.aiju.fun/"><img src="https://avatars.githubusercontent.com/u/18513362?v=4?s=100" width="100px;" alt="Carlo"/><br /><sub><b>Carlo</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=a67793581" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/hrz394943230"><img src="https://avatars.githubusercontent.com/u/28583005?v=4?s=100" width="100px;" alt="hrz"/><br /><sub><b>hrz</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=hrz394943230" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/ctrlz526"><img src="https://avatars.githubusercontent.com/u/143257420?v=4?s=100" width="100px;" alt="Ctrlz"/><br /><sub><b>Ctrlz</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=ctrlz526" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/flippancy"><img src="https://avatars.githubusercontent.com/u/6467703?v=4?s=100" width="100px;" alt="flippancy"/><br /><sub><b>flippancy</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=flippancy" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/Infinityay"><img src="https://avatars.githubusercontent.com/u/103165980?v=4?s=100" width="100px;" alt="Yuhang Lu"/><br /><sub><b>Yuhang Lu</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=Infinityay" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://triepod.ai"><img src="https://avatars.githubusercontent.com/u/199543909?v=4?s=100" width="100px;" alt="Bryan Thompson"/><br /><sub><b>Bryan Thompson</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=bryankthompson" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="http://www.megvii.com"><img src="https://avatars.githubusercontent.com/u/7806992?v=4?s=100" width="100px;" alt="tan jun"/><br /><sub><b>tan jun</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=tanxxjun321" title="Code">💻</a></td>
    </tr>
    <tr>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/coldmountein"><img src="https://avatars.githubusercontent.com/u/95873096?v=4?s=100" width="100px;" alt="coldmountain"/><br /><sub><b>coldmountain</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=coldmountein" title="Code">💻</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://blog.litpp.com/"><img src="https://avatars.githubusercontent.com/u/44826388?v=4?s=100" width="100px;" alt="mamage"/><br /><sub><b>mamage</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=yqdaddy" title="Code">💻</a> <a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=yqdaddy" title="Documentation">📖</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://runyang.vercel.app/"><img src="https://avatars.githubusercontent.com/u/54588936?v=4?s=100" width="100px;" alt="Runyang YOU"/><br /><sub><b>Runyang YOU</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=YRYangang" title="Code">💻</a> <a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=YRYangang" title="Documentation">📖</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://www.hnfnu.edu.cn/"><img src="https://avatars.githubusercontent.com/u/134906805?v=4?s=100" width="100px;" alt="e0_7"/><br /><sub><b>e0_7</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=Daily-AC" title="Code">💻</a> <a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=Daily-AC" title="Documentation">📖</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/prehisle"><img src="https://avatars.githubusercontent.com/u/2081344?v=4?s=100" width="100px;" alt="prehisle"/><br /><sub><b>prehisle</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=prehisle" title="Code">💻</a> <a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=prehisle" title="Documentation">📖</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/blablabiu"><img src="https://avatars.githubusercontent.com/u/123888078?v=4?s=100" width="100px;" alt="Xinhao Chen"/><br /><sub><b>Xinhao Chen</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=blablabiu" title="Code">💻</a> <a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=blablabiu" title="Documentation">📖</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/qiuxsgit"><img src="https://avatars.githubusercontent.com/u/15036686?v=4?s=100" width="100px;" alt="qiuxsgit"/><br /><sub><b>qiuxsgit</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=qiuxsgit" title="Code">💻</a></td>
    </tr>
    <tr>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/ZhuYichuan"><img src="https://avatars.githubusercontent.com/u/7954801?v=4?s=100" width="100px;" alt="openlts"/><br /><sub><b>openlts</b></sub></a><br /><a href="https://github.com/xpzouying/xiaohongshu-mcp/commits?author=ZhuYichuan" title="Code">💻</a></td>
    </tr>
  </tbody>
</table>

<!-- markdownlint-restore -->
<!-- prettier-ignore-end -->

<!-- ALL-CONTRIBUTORS-LIST:END -->

### ✨ 特别感谢

<table>
  <tbody>
    <tr>
      <td align="center" valign="top" width="20%"><a href="https://github.com/wanpengxie"><img src="https://avatars.githubusercontent.com/wanpengxie" width="130px;" alt="wanpengxie"/><br /><sub><b>@wanpengxie</b></sub></a></td>
      <td align="center" valign="top" width="20%"><a href="https://github.com/tanxxjun321"><img src="https://avatars.githubusercontent.com/u/7806992?v=4" width="130px;" alt="tanxxjun321"/><br /><sub><b>@tanxxjun321</b></sub></a></td>
      <td align="center" valign="top" width="20%"><a href="https://github.com/Angiin"><img src="https://avatars.githubusercontent.com/u/17389304?v=4" width="130px;" alt="Angiin"/><br /><sub><b>@Angiin</b></sub></a></td>
    </tr>
  </tbody>
</table>

本项目遵循 [all-contributors](https://github.com/all-contributors/all-contributors) 规范。欢迎任何形式的贡献！

## 📄 许可证

本仓库衍生自 [xpzouying/xiaohongshu-mcp](https://github.com/xpzouying/xiaohongshu-mcp)，
整体采用 [Apache License 2.0](LICENSE) 开源——与上游同一协议，原始版权声明原样保留。

你可以自由地使用、修改和分发本项目，包括用于商业用途，只需保留版权声明和许可证文件。

**关于 MIT**：本仓库新增的原创文件（`wait.go`、`browser_lease.go`、`subtitle.go`、
`risk.go`、`verification.go`、`verify_link.go` 等，完整清单见 [NOTICE](NOTICE)）
在 Apache-2.0 之外**同时**以 [MIT](LICENSE-MIT) 授权，方便别的项目单独取用其中某个文件。
但把它们与本仓库其余部分一并使用或分发时，整体仍受 Apache-2.0 约束——
双许可只在这些文件被单独取出时才有意义。

被修改过的上游文件清单，按 Apache-2.0 第 4(b) 条记在 [NOTICE](NOTICE) 里。

向本仓库提交的贡献，默认按 Apache-2.0 授权。
