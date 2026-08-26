package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/headless_browser"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"github.com/xpzouying/xiaohongshu-mcp/configs"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/downloader"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/xhsutil"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// XiaohongshuService 小红书业务服务
type XiaohongshuService struct {
	// scans 登录扫码和安全验证扫码共用的待扫码登记表。
	//
	// 两者是同一个形状的问题：发一张码、留一个浏览器等结果，所以共用 loginSessions
	// 那套「同一时刻只留一个」的约束。必须共用而不是各记各的：分开的话同一时刻可以
	// 有两个纯等待的浏览器，正好等于默认名额上限 2，闸门被整体关死——而这两个浏览器
	// 什么都不干，只在等同一部手机扫码。共用之后待扫码会话最多占掉一张票。
	scans loginSessions

	// links 一次性验证链接的登记表，见 verify_link.go。
	links verifyLinks
}

// NewXiaohongshuService 创建小红书服务实例
func NewXiaohongshuService() *XiaohongshuService {
	return &XiaohongshuService{}
}

// PublishRequest 发布请求
type PublishRequest struct {
	Title      string   `json:"title" binding:"required"`
	Content    string   `json:"content" binding:"required"`
	Images     []string `json:"images" binding:"required,min=1"`
	Tags       []string `json:"tags,omitempty"`
	ScheduleAt string   `json:"schedule_at,omitempty"` // 定时发布时间，ISO8601格式，为空则立即发布
	IsOriginal bool     `json:"is_original,omitempty"` // 是否声明原创
	Visibility string   `json:"visibility,omitempty"`  // 可见范围: "公开可见"(默认), "仅自己可见", "仅互关好友可见"
	Products   []string `json:"products,omitempty"`    // 商品关键词列表，用于绑定带货商品
}

// LoginStatusResponse 登录状态响应
type LoginStatusResponse struct {
	IsLoggedIn bool   `json:"is_logged_in"`
	Username   string `json:"username,omitempty"` // 当前登录账号的昵称
	UserID     string `json:"user_id,omitempty"`  // 用户唯一标识（个人主页 URL 中的 ID）
}

// LoginQrcodeResponse 登录扫码二维码
type LoginQrcodeResponse struct {
	Timeout    string `json:"timeout"`
	IsLoggedIn bool   `json:"is_logged_in"`
	Img        string `json:"img,omitempty"`
}

// VerificationQrcodeResponse 安全验证扫码二维码。Blocked 为 false 时只有 Message 有值。
type VerificationQrcodeResponse struct {
	Blocked    bool   `json:"blocked"`
	Message    string `json:"message"`
	Timeout    string `json:"timeout,omitempty"`
	Img        string `json:"img,omitempty"`
	PageURL    string `json:"page_url,omitempty"`
	VerifyType string `json:"verify_type,omitempty"`
	VerifyBiz  string `json:"verify_biz,omitempty"`

	// VerifyURL 浏览器直达的一次性验证网页。没配 XHS_PUBLIC_BASE_URL 时为空——
	// 服务端猜不到自己的公网地址，宁可不给，也不能给一条点不开的。
	VerifyURL string `json:"verify_url,omitempty"`
	// VerifyPath 上面那条链接的相对路径，任何情况下都有，供用户自己拼。
	VerifyPath string `json:"verify_path,omitempty"`
}

// PublishResponse 发布响应
type PublishResponse struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Images  int    `json:"images"`
	Status  string `json:"status"`
}

// PublishVideoRequest 发布视频请求（仅支持本地单个视频文件）
type PublishVideoRequest struct {
	Title      string   `json:"title" binding:"required"`
	Content    string   `json:"content" binding:"required"`
	Video      string   `json:"video" binding:"required"`
	Tags       []string `json:"tags,omitempty"`
	ScheduleAt string   `json:"schedule_at,omitempty"` // 定时发布时间，ISO8601格式，为空则立即发布
	Visibility string   `json:"visibility,omitempty"`  // 可见范围: "公开可见"(默认), "仅自己可见", "仅互关好友可见"
	Products   []string `json:"products,omitempty"`    // 商品关键词列表，用于绑定带货商品
}

// PublishVideoResponse 发布视频响应
type PublishVideoResponse struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Video   string `json:"video"`
	Status  string `json:"status"`
}

// FeedsListResponse Feeds列表响应
type FeedsListResponse struct {
	Feeds []xiaohongshu.Feed `json:"feeds"`
	Count int                `json:"count"`
}

// UserProfileResponse 用户主页响应
type UserProfileResponse struct {
	UserBasicInfo xiaohongshu.UserBasicInfo      `json:"userBasicInfo"`
	Interactions  []xiaohongshu.UserInteractions `json:"interactions"`
	Feeds         []xiaohongshu.Feed             `json:"feeds"`
}

// DeleteCookies 删除 cookies 文件，用于登录重置
func (s *XiaohongshuService) DeleteCookies(ctx context.Context) error {
	cookiePath := cookies.GetCookiesFilePath()
	cookieLoader := cookies.NewLoadCookie(cookiePath)
	return cookieLoader.DeleteCookies()
}

// CheckLoginStatus 检查登录状态
func (s *XiaohongshuService) CheckLoginStatus(ctx context.Context) (*LoginStatusResponse, error) {
	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	loginAction := xiaohongshu.NewLogin(page)

	isLoggedIn, err := loginAction.CheckLoginStatus(ctx)
	if err != nil {
		return nil, err
	}

	response := &LoginStatusResponse{
		IsLoggedIn: isLoggedIn,
	}

	// 已登录时从当前页读取真实账号信息；读不到只记 warn，不影响状态返回。
	if isLoggedIn {
		if user, err := loginAction.CurrentUser(ctx); err != nil {
			logrus.Warnf("failed to get current user info: %v", err)
		} else {
			response.Username = user.Nickname
			response.UserID = user.UserID
		}
	}

	return response, nil
}

// GetLoginQrcode 获取登录的扫码二维码
func (s *XiaohongshuService) GetLoginQrcode(ctx context.Context) (*LoginQrcodeResponse, error) {
	// 先把上一轮还挂着的待扫码会话放掉，再去排队取名额票，顺序不能反。理由和
	// GetVerificationQrcode 那段一字不差：旧会话钉着的就是一张票，而 scans.start
	// 排在取票之后——新会话必须先抢到另一张票，才轮得到它去关掉那个已经彻底没用的
	// 旧会话。名额只有 2 张（XHS_BROWSER_CONCURRENCY=1 时更是必然失败），
	// 这就是标准的 acquire-before-release 自饿死。
	s.scans.cancelPending()

	b := newBrowser(ctx)

	// 这是唯一一个浏览器要活过函数返回的方法（后台等扫码），所以不能简单 defer 关闭。
	// 但也不能等某个分支确定了再 defer：这中间每一步都可能 panic——b.NewPage() 用的是
	// rod 的 MustPage，FetchQrcodeImage 内部也是 Must*——那时 defer 还没挂上，整个
	// 浏览器进程组（实测约 300MB）连同它占的那张名额票就永久泄漏了。名额只有 2 张，
	// 漏两次闸门就彻底关死，之后每个请求都只能排队到超时。改成默认关闭 + 显式移交。
	//
	// page 用变量捕获而不是参数：defer 要赶在 NewPage 之前挂上，那时还没有 page。
	var page *rod.Page
	deferFunc := closeQuietly("登录扫码", func() {
		if page != nil {
			_ = page.Close()
		}
		b.Close()
	})

	handedOff := false
	defer func() {
		if !handedOff {
			deferFunc()
		}
	}()

	page = b.NewPage()

	loginAction := xiaohongshu.NewLogin(page)

	img, loggedIn, err := loginAction.FetchQrcodeImage(ctx)
	if err != nil {
		return nil, err
	}

	timeout := 4 * time.Minute

	if !loggedIn {
		// 所有权交给后台 goroutine，由它负责关闭
		s.waitScanInBackground(loginAction, page, deferFunc, timeout)
		handedOff = true
	}

	return &LoginQrcodeResponse{
		Timeout: func() string {
			if loggedIn {
				return "0s"
			}
			return timeout.String()
		}(),
		Img:        img,
		IsLoggedIn: loggedIn,
	}, nil
}

// waitScanInBackground 在后台等用户扫码，扫上了就存 cookie。
//
// 浏览器必须一直活着才检测得到扫码，所以这里不能提前关；但也不能任由它堆积——
// 再取一次二维码就会把上一个还在等的会话关掉，同一时刻只留一个。
func (s *XiaohongshuService) waitScanInBackground(
	loginAction *xiaohongshu.LoginAction, page *rod.Page, closeBrowser func(), timeout time.Duration,
) {
	ctxTimeout, cancel := context.WithTimeout(context.Background(), timeout)

	// released 等到浏览器真的关掉、名额票真的还回去才关闭，供 cancelPending 等待。
	released := make(chan struct{})

	seq := s.scans.start(cancel, released)
	logrus.Infof("等待扫码登录，会话 #%d，超时 %s", seq, timeout)

	go func() {
		// 这是一条裸 goroutine，没有 gin.Recovery 也没有 withPanicRecovery 兜着，
		// 漏一个 panic 出去就是整个进程退出——连带另外那张名额票和所有在途请求。
		// 注册在最前面，于是执行时排在最后：closeBrowser 自己 panic（Chromium 先崩了
		// 的话 rod 的 MustClose 就是 panic）也接得住。
		defer func() {
			if r := recover(); r != nil {
				logrus.Errorf("等待扫码登录的后台会话 #%d 出错: %v", seq, r)
			}
		}()
		defer close(released)
		defer closeBrowser()
		defer cancel()
		defer s.scans.finish(seq)

		if loginAction.WaitForLogin(ctxTimeout) {
			if err := saveCookies(page); err != nil {
				logrus.Errorf("扫码成功但保存 cookies 失败，会话 #%d: %v", seq, err)
				return
			}
			logrus.Infof("扫码登录成功，cookies 已保存，会话 #%d", seq)
			return
		}

		// 没等到扫码：要么超时，要么被新取的二维码取代
		logrus.Infof("登录会话 #%d 结束，未检测到扫码（超时或已被新的二维码取代）", seq)
	}()
}

// pageCloseTimeout 关页面的上限，理由同 cookieReadTimeout：走的是 browser 那条
// context.Background()，不设上限就可能挂在归还名额票之前。
const pageCloseTimeout = 5 * time.Second

// closeQuietly 把「关浏览器」包成绝不 panic 出去的形状。
//
// headless_browser.Close 走的是 rod 的 MustClose：Chromium 自己先崩了（容器 OOM
// 先杀掉子进程是最现实的触发条件）时这一步是 panic 而不是 error。而这些 closeBrowser
// 全都在裸 goroutine 的 defer 里执行，没有 gin.Recovery 也没有 withPanicRecovery
// 兜着，漏出去就是整个进程退出。在构造处包一次，三条路径（登录扫码、取验证码、
// 验证网页会话）一起受益。
//
// 名额票不受影响：leasedBrowser.Close 用 defer 还票，panic 也还得掉。
func closeQuietly(what string, closeBrowser func()) func() {
	return func() {
		defer func() {
			if r := recover(); r != nil {
				logrus.Errorf("关闭%s的浏览器时出错: %v", what, r)
			}
		}()

		closeBrowser()
	}
}

// GetVerificationQrcode 取安全验证二维码，透出给账号本人用小红书 App 扫。
//
// 这是搜索被拦之后唯一走得通的出路：验证页跟本次 headless 会话绑定，会话一销毁，
// 那条 URL 就作废了，用户自己打开只会拿到另一个 verifyUuid 的新挑战。
//
// 生命周期照抄 GetLoginQrcode：浏览器要活过函数返回（后台等扫码结果并存 cookie），
// 所以不能简单 defer 关闭，也不能等分支确定了再 defer——中间每一步都可能 panic
// （b.NewPage() 是 rod 的 MustPage），那时 defer 还没挂上，整个浏览器进程组连同它
// 占的那张名额票就永久泄漏了。名额只有 2 张，漏两次闸门彻底关死。默认关闭 + 显式移交。
func (s *XiaohongshuService) GetVerificationQrcode(ctx context.Context) (*VerificationQrcodeResponse, error) {
	// 先把上一轮还挂着的待扫码会话放掉，再去排队取名额票，顺序不能反：那个会话钉着
	// 的就是一张票，而验证页自述二维码 1 分钟失效、工具文案又明确要求「过期就重新调
	// 一次本工具」。反过来的话，新会话必须先抢到另一张票才轮得到它去关掉那个已经
	// 彻底没用的旧会话——挡住重试的正是它自己的僵尸会话。
	s.scans.cancelPending()

	b := newBrowser(ctx)

	// page 用变量捕获而不是参数：defer 要赶在 NewPage 之前挂上，那时还没有 page。
	var page *rod.Page
	deferFunc := closeQuietly("安全验证扫码", func() {
		if page != nil {
			// 关页面同样要有上限：Page.Close 要等 TargetDestroyed 事件回来，走的还是
			// browser 那条 context.Background()。这一步排在归还名额票之前，挂住就是漏票。
			_ = page.Timeout(pageCloseTimeout).Close()
		}
		b.Close()
	})

	handedOff := false
	defer func() {
		if !handedOff {
			deferFunc()
		}
	}()

	page = b.NewPage()

	action := xiaohongshu.NewVerificationAction(page)

	qrcode, err := action.FetchQrcode(ctx)
	if err != nil {
		return nil, err
	}

	if !qrcode.Blocked {
		// 没被拦就没有码可透出，浏览器立刻关掉：这条路径上多留一秒都是白占名额票。
		//
		// 但话不能说成「无需验证」：这个分支同时盖着登录墙和任何没被识别出来的拦截
		// 形态，一句「无需验证」会让调用方认定风控没问题，然后原地重试到死。带上
		// 页面落点和下一步动作，至少能自己往下走一格。
		return &VerificationQrcodeResponse{
			Message: notBlockedMessage(qrcode.PageURL),
			PageURL: qrcode.PageURL,
		}, nil
	}

	// 发一条浏览器直达链接，并且把**这个已经打开着验证页的会话**直接挂到它上面。
	//
	// 「挂上去」是这条路径的全部要害。从前这里只登记 token、让网页第一次拉 /state
	// 时自己再起一套浏览器，而那意味着「调工具拿链接 → 手机点开链接」——本功能最
	// 主要、最自然的路径——会在十几秒内第二次导航到验证页。实测小红书对验证页本身
	// 限流：第二次导航拿回来的页面标题还是「安全验证」，正文却变成「请求太频繁，
	// 请一分钟后再试」，上面根本没有二维码可读，只能等到超时报错。复用之后网页第一
	// 次拉 /state 只是给这个会话打一拍心跳，全程一次导航。
	//
	// 会话也不会因为没人点链接就白占名额票：首拍心跳到达之前用的是
	// verifyFirstBeatGrace(90s)，跟从前只有内联图那条路时的等待上限一模一样。
	link, lerr := s.links.issue()
	if lerr != nil {
		// 发链接失败不影响上面那张已经取到的码，会话照建：它是唯一存得下验证结果
		// 的地方（cookie jar 在浏览器里，关掉就没了）。只是没人查得到这条链接，
		// 于是也没人 poll 它，首拍宽限到期后自己拆掉。
		logrus.Errorf("生成验证链接失败，本次只走内联二维码那条路: %v", lerr)
		link = newVerifyLink()
	}

	// 所有权交给会话，由它负责关闭浏览器
	sess := newVerifySession(s, link, &verificationProbe{action: action, page: page}, deferFunc)
	handedOff = true

	link.attach(sess, qrcode.Img)

	verifyURL, verifyPath := "", ""
	if lerr == nil {
		verifyURL, verifyPath = verifyLinkAddress(link.token)
	}

	message := "检测到小红书安全验证"
	if qrcode.VerifyType != "" || qrcode.VerifyBiz != "" {
		message += fmt.Sprintf("（verifyType=%s verifyBiz=%s）", qrcode.VerifyType, qrcode.VerifyBiz)
	}

	return &VerificationQrcodeResponse{
		Blocked: true,
		Message: message,
		// 报的是「没人打开验证网页」时的上限。打开了那条链接就改由页面心跳续命，
		// 上限跟着变长（见 verify_link.go 的 verifyBeatTimeout / verifySessionMaxLife），
		// 而这句话是说给「只看对话里这张内联图」的人听的。
		Timeout:    verifyFirstBeatGrace.String(),
		Img:        qrcode.Img,
		PageURL:    qrcode.PageURL,
		VerifyType: qrcode.VerifyType,
		VerifyBiz:  qrcode.VerifyBiz,
		VerifyURL:  verifyURL,
		VerifyPath: verifyPath,
	}, nil
}

// 这里从前有一个 waitVerifyInBackground：起一条不挂在任何链接上的后台 goroutine，
// 只管等 WaitVerified、存 cookie。它已经被 verifySession 整个顶替——同一个会话现在
// 既负责等扫码存 cookie，又负责给验证网页保鲜、换码、报状态，于是「取码」和「打开
// 链接」共用一次导航。留着两套等于留着两条导航路径，而第二次导航必被限流。
//
// verifySession 的收尾比它更全：拆浏览器有 recover 兜底、名额票有 released 信号、
// 首拍心跳前后两套预算。

// notBlockedMessage 「这次没被拦」的说法。MCP/REST 和验证网页共用一套措辞。
//
// 话不能说成「无需验证」：这个分支同时盖着登录墙和任何没被识别出来的拦截形态，
// 一句「无需验证」会让调用方认定风控没问题，然后原地重试到死。
func notBlockedMessage(pageURL string) string {
	message := "当前这次访问未被安全验证拦截"
	if pageURL != "" {
		message += fmt.Sprintf("（页面停在 %s）", pageURL)
	}

	return message + "；若原操作仍然失败，请先用 check_login_status 确认登录态，或直接重试原操作"
}

// startVerifySession 给验证网页**重新**起一个待扫码浏览器会话。
//
// 这不是主路径：主路径上会话在 GetVerificationQrcode 里就建好并挂到链接上了，网页
// 只是给它打心跳。走到这里说明那个会话已经不在了——心跳断过一次（用户切去小红书
// App 扫码、切回来），或者上一次被限流退避到点了。也就是说这里会再导航一次验证页，
// 而站点对验证页限流：这条路径本身就是限流的主要来源，能不走就不走。
//
// 返回 (nil, qrcode, nil) 表示这次访问根本没被拦，浏览器已经关掉了。
//
// 生命周期同 GetVerificationQrcode：默认关闭 + 显式移交。中间每一步都可能 panic
// （b.NewPage() 是 rod 的 MustPage），defer 没先挂上就是整个浏览器进程组连同它占的
// 那张名额票永久泄漏——名额只有 2 张，漏两次闸门彻底关死。
func (s *XiaohongshuService) startVerifySession(
	ctx context.Context, link *verifyLink,
) (*verifySession, *xiaohongshu.VerificationQrcode, error) {
	// 先放掉上一轮还挂着的待扫码会话，再去排队取名额票，顺序不能反：那个会话钉着
	// 的就是一张票，反过来就是标准的 acquire-before-release 自饿死。
	s.scans.cancelPending()

	b := newBrowser(ctx)

	var page *rod.Page
	deferFunc := closeQuietly("验证网页会话", func() {
		if page != nil {
			_ = page.Timeout(pageCloseTimeout).Close()
		}
		b.Close()
	})

	handedOff := false
	defer func() {
		if !handedOff {
			deferFunc()
		}
	}()

	page = b.NewPage()

	action := xiaohongshu.NewVerificationAction(page)

	qrcode, err := action.FetchQrcode(ctx)
	if err != nil {
		return nil, nil, err
	}

	if !qrcode.Blocked {
		return nil, qrcode, nil
	}

	sess := newVerifySession(s, link, &verificationProbe{action: action, page: page}, deferFunc)
	handedOff = true

	return sess, qrcode, nil
}

// verificationProbe verifyProbe 的生产实现，把会话循环要用的三件事包在一起。
type verificationProbe struct {
	action *xiaohongshu.VerificationAction
	page   *rod.Page
}

func (p *verificationProbe) Live(ctx context.Context) (*xiaohongshu.LiveQrcode, error) {
	return p.action.Live(ctx)
}

func (p *verificationProbe) Verified() bool { return p.action.Verified() }

func (p *verificationProbe) SaveCookies() error { return saveCookies(p.page) }

// PublishContent 发布内容
func (s *XiaohongshuService) PublishContent(ctx context.Context, req *PublishRequest) (*PublishResponse, error) {
	// 验证标题长度（小红书限制：最大20个字）
	if xhsutil.CalcTitleLength(req.Title) > 20 {
		return nil, fmt.Errorf("标题长度超过限制")
	}

	imagePaths, err := s.processImages(req.Images)
	if err != nil {
		return nil, err
	}

	var scheduleTime *time.Time
	if req.ScheduleAt != "" {
		t, err := time.Parse(time.RFC3339, req.ScheduleAt)
		if err != nil {
			return nil, fmt.Errorf("定时发布时间格式错误，请使用 ISO8601 格式: %v", err)
		}

		// 校验定时发布时间范围：1小时至14天
		now := time.Now()
		minTime := now.Add(1 * time.Hour)
		maxTime := now.Add(14 * 24 * time.Hour)

		if t.Before(minTime) {
			return nil, fmt.Errorf("定时发布时间必须至少在1小时后，当前设置: %s，最早可选: %s",
				t.Format("2006-01-02 15:04"), minTime.Format("2006-01-02 15:04"))
		}
		if t.After(maxTime) {
			return nil, fmt.Errorf("定时发布时间不能超过14天，当前设置: %s，最晚可选: %s",
				t.Format("2006-01-02 15:04"), maxTime.Format("2006-01-02 15:04"))
		}

		scheduleTime = &t
		logrus.Infof("设置定时发布时间: %s", t.Format("2006-01-02 15:04"))
	}

	content := xiaohongshu.PublishImageContent{
		Title:        req.Title,
		Content:      req.Content,
		Tags:         req.Tags,
		ImagePaths:   imagePaths,
		ScheduleTime: scheduleTime,
		IsOriginal:   req.IsOriginal,
		Visibility:   req.Visibility,
		Products:     req.Products,
	}

	if err := s.publishContent(ctx, content); err != nil {
		logrus.Errorf("发布内容失败: title=%s %v", content.Title, err)
		return nil, err
	}

	response := &PublishResponse{
		Title:   req.Title,
		Content: req.Content,
		Images:  len(imagePaths),
		Status:  "发布完成",
	}

	return response, nil
}

// processImages 处理图片列表，支持URL下载和本地路径
func (s *XiaohongshuService) processImages(images []string) ([]string, error) {
	processor := downloader.NewImageProcessor()
	return processor.ProcessImages(images)
}

// publishContent 执行内容发布
func (s *XiaohongshuService) publishContent(ctx context.Context, content xiaohongshu.PublishImageContent) error {
	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action, err := xiaohongshu.NewPublishImageAction(page)
	if err != nil {
		return err
	}

	return action.Publish(ctx, content)
}

// PublishVideo 发布视频（本地文件）
func (s *XiaohongshuService) PublishVideo(ctx context.Context, req *PublishVideoRequest) (*PublishVideoResponse, error) {
	// 标题长度校验（小红书限制：最大20个字）
	if xhsutil.CalcTitleLength(req.Title) > 20 {
		return nil, fmt.Errorf("标题长度超过限制")
	}

	// 本地视频文件校验
	if req.Video == "" {
		return nil, fmt.Errorf("必须提供本地视频文件")
	}
	if _, err := os.Stat(req.Video); err != nil {
		return nil, fmt.Errorf("视频文件不存在或不可访问: %v", err)
	}

	var scheduleTime *time.Time
	if req.ScheduleAt != "" {
		t, err := time.Parse(time.RFC3339, req.ScheduleAt)
		if err != nil {
			return nil, fmt.Errorf("定时发布时间格式错误，请使用 ISO8601 格式: %v", err)
		}

		// 校验定时发布时间范围：1小时至14天
		now := time.Now()
		minTime := now.Add(1 * time.Hour)
		maxTime := now.Add(14 * 24 * time.Hour)

		if t.Before(minTime) {
			return nil, fmt.Errorf("定时发布时间必须至少在1小时后，当前设置: %s，最早可选: %s",
				t.Format("2006-01-02 15:04"), minTime.Format("2006-01-02 15:04"))
		}
		if t.After(maxTime) {
			return nil, fmt.Errorf("定时发布时间不能超过14天，当前设置: %s，最晚可选: %s",
				t.Format("2006-01-02 15:04"), maxTime.Format("2006-01-02 15:04"))
		}

		scheduleTime = &t
		logrus.Infof("设置定时发布时间: %s", t.Format("2006-01-02 15:04"))
	}

	content := xiaohongshu.PublishVideoContent{
		Title:        req.Title,
		Content:      req.Content,
		Tags:         req.Tags,
		VideoPath:    req.Video,
		ScheduleTime: scheduleTime,
		Visibility:   req.Visibility,
		Products:     req.Products,
	}

	if err := s.publishVideo(ctx, content); err != nil {
		return nil, err
	}

	resp := &PublishVideoResponse{
		Title:   req.Title,
		Content: req.Content,
		Video:   req.Video,
		Status:  "发布完成",
	}
	return resp, nil
}

// publishVideo 执行视频发布
func (s *XiaohongshuService) publishVideo(ctx context.Context, content xiaohongshu.PublishVideoContent) error {
	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action, err := xiaohongshu.NewPublishVideoAction(page)
	if err != nil {
		return err
	}

	return action.PublishVideo(ctx, content)
}

// ListFeeds 获取Feeds列表
func (s *XiaohongshuService) ListFeeds(ctx context.Context) (*FeedsListResponse, error) {
	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewFeedsListAction(page)

	feeds, err := action.GetFeedsList(ctx)
	if err != nil {
		logrus.Errorf("获取 Feeds 列表失败: %v", err)
		return nil, err
	}

	response := &FeedsListResponse{
		Feeds: feeds,
		Count: len(feeds),
	}

	return response, nil
}

func (s *XiaohongshuService) SearchFeeds(ctx context.Context, keyword string, filters ...xiaohongshu.FilterOption) (*FeedsListResponse, error) {
	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewSearchAction(page)

	feeds, err := action.Search(ctx, keyword, filters...)
	if err != nil {
		return nil, err
	}

	response := &FeedsListResponse{
		Feeds: feeds,
		Count: len(feeds),
	}

	return response, nil
}

// GetFeedDetail 获取Feed详情
func (s *XiaohongshuService) GetFeedDetail(ctx context.Context, feedID, xsecToken string, loadAllComments bool) (*FeedDetailResponse, error) {
	return s.GetFeedDetailWithConfig(ctx, feedID, xsecToken, loadAllComments, xiaohongshu.DefaultCommentLoadConfig())
}

// GetFeedDetailWithConfig 使用配置获取Feed详情
func (s *XiaohongshuService) GetFeedDetailWithConfig(ctx context.Context, feedID, xsecToken string, loadAllComments bool, config xiaohongshu.CommentLoadConfig) (*FeedDetailResponse, error) {
	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewFeedDetailAction(page)

	result, err := action.GetFeedDetailWithConfig(ctx, feedID, xsecToken, loadAllComments, config)
	if err != nil {
		return nil, err
	}

	response := &FeedDetailResponse{
		FeedID: feedID,
		Data:   result,
	}

	return response, nil
}

// UserProfile 获取用户信息
func (s *XiaohongshuService) UserProfile(ctx context.Context, userID, xsecToken, tab string) (*UserProfileResponse, error) {
	parsed, err := xiaohongshu.ParseProfileTab(tab)
	if err != nil {
		return nil, err
	}

	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewUserProfileAction(page)

	result, err := action.UserProfile(ctx, userID, xsecToken, parsed)
	if err != nil {
		return nil, err
	}
	response := &UserProfileResponse{
		UserBasicInfo: result.UserBasicInfo,
		Interactions:  result.Interactions,
		Feeds:         result.Feeds,
	}

	return response, nil

}

// PostCommentToFeed 发表评论到Feed
func (s *XiaohongshuService) PostCommentToFeed(ctx context.Context, feedID, xsecToken, content string) (*PostCommentResponse, error) {
	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewCommentFeedAction(page)

	if err := action.PostComment(ctx, feedID, xsecToken, content); err != nil {
		return nil, err
	}

	return &PostCommentResponse{FeedID: feedID, Success: true, Message: "评论发表成功"}, nil
}

// LikeFeed 点赞笔记
func (s *XiaohongshuService) LikeFeed(ctx context.Context, feedID, xsecToken string) (*ActionResult, error) {
	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewLikeAction(page)
	if err := action.Like(ctx, feedID, xsecToken); err != nil {
		return nil, err
	}
	return &ActionResult{FeedID: feedID, Success: true, Message: "点赞成功或已点赞"}, nil
}

// UnlikeFeed 取消点赞笔记
func (s *XiaohongshuService) UnlikeFeed(ctx context.Context, feedID, xsecToken string) (*ActionResult, error) {
	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewLikeAction(page)
	if err := action.Unlike(ctx, feedID, xsecToken); err != nil {
		return nil, err
	}
	return &ActionResult{FeedID: feedID, Success: true, Message: "取消点赞成功或未点赞"}, nil
}

// FavoriteFeed 收藏笔记
func (s *XiaohongshuService) FavoriteFeed(ctx context.Context, feedID, xsecToken string) (*ActionResult, error) {
	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewFavoriteAction(page)
	if err := action.Favorite(ctx, feedID, xsecToken); err != nil {
		return nil, err
	}
	return &ActionResult{FeedID: feedID, Success: true, Message: "收藏成功或已收藏"}, nil
}

// UnfavoriteFeed 取消收藏笔记
func (s *XiaohongshuService) UnfavoriteFeed(ctx context.Context, feedID, xsecToken string) (*ActionResult, error) {
	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewFavoriteAction(page)
	if err := action.Unfavorite(ctx, feedID, xsecToken); err != nil {
		return nil, err
	}
	return &ActionResult{FeedID: feedID, Success: true, Message: "取消收藏成功或未收藏"}, nil
}

// ReplyCommentToFeed 回复指定评论
func (s *XiaohongshuService) ReplyCommentToFeed(ctx context.Context, feedID, xsecToken, commentID, userID, content string) (*ReplyCommentResponse, error) {
	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewCommentFeedAction(page)

	if err := action.ReplyToComment(ctx, feedID, xsecToken, commentID, userID, content); err != nil {
		return nil, err
	}

	return &ReplyCommentResponse{
		FeedID:          feedID,
		TargetCommentID: commentID,
		TargetUserID:    userID,
		Success:         true,
		Message:         "评论回复成功",
	}, nil
}

// GetUnreadCount 获取通知未读数
func (s *XiaohongshuService) GetUnreadCount(ctx context.Context) (*xiaohongshu.NotificationCount, error) {
	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	return xiaohongshu.NewNotificationAction(page).UnreadCount(ctx)
}

// ListNotifications 获取指定分区的通知列表
func (s *XiaohongshuService) ListNotifications(ctx context.Context, tab string, limit int) (*xiaohongshu.NotificationList, error) {
	parsed, err := xiaohongshu.ParseNotificationTab(tab)
	if err != nil {
		return nil, err
	}

	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	return xiaohongshu.NewNotificationAction(page).List(ctx, parsed, limit)
}

// LikeNotification 给通知里的评论点赞或取消点赞
func (s *XiaohongshuService) LikeNotification(ctx context.Context, commentID string, unlike bool) (*xiaohongshu.NotificationLikeResult, error) {
	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	return xiaohongshu.NewNotificationAction(page).Like(ctx, commentID, unlike)
}

// ReplyNotification 在通知页就地回复评论
func (s *XiaohongshuService) ReplyNotification(ctx context.Context, commentID, content string) (*xiaohongshu.NotificationReplyResult, error) {
	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	return xiaohongshu.NewNotificationAction(page).Reply(ctx, commentID, content)
}

// newBrowser 建一个浏览器实例，受并发闸门约束（见 browser_lease.go）。
//
// 返回的是 *leasedBrowser 而非裸 *headless_browser.Browser：它内嵌了后者，
// 调用方用到的 NewPage 由方法提升直接透传，只有 Close 被拦下来顺便归还名额，
// 所以 19 个调用点一处都不用改。
func newBrowser(ctx context.Context) *leasedBrowser {
	return newLeasedBrowser(ctx, func() *headless_browser.Browser {
		return browser.NewBrowser(configs.IsHeadless(),
			browser.WithFingerprintSeed(configs.FingerprintSeed()),
			browser.WithProxy(configs.Proxy()),
		)
	})
}

// cookieReadTimeout 读 cookie 的上限。
//
// GetCookies 走的是 browser 的 ctx，而 headless_browser 用 rod.New() 从不设 Context——
// 那就是 context.Background()，既没有超时也不响应取消。两个调用点都在后台 goroutine 里，
// 且都排在归还浏览器名额票之前：CDP 迟迟不回就永久阻塞，那张票再也回不来。名额只有
// 2 张（见 browser_lease.go），漏两次闸门就彻底关死，之后每个请求都只能排队到超时。
const cookieReadTimeout = 10 * time.Second

func saveCookies(page *rod.Page) error {
	cks, err := page.Browser().Timeout(cookieReadTimeout).GetCookies()
	if err != nil {
		return err
	}

	data, err := json.Marshal(cks)
	if err != nil {
		return err
	}

	cookieLoader := cookies.NewLoadCookie(cookies.GetCookiesFilePath())
	return cookieLoader.SaveCookies(data)
}

// withBrowserPage 执行需要浏览器页面的操作的通用函数
func withBrowserPage(ctx context.Context, fn func(*rod.Page) error) error {
	b := newBrowser(ctx)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	return fn(page)
}

// GetMyProfile 获取当前登录用户的个人信息
func (s *XiaohongshuService) GetMyProfile(ctx context.Context, tab string) (*UserProfileResponse, error) {
	parsed, err := xiaohongshu.ParseProfileTab(tab)
	if err != nil {
		return nil, err
	}

	var result *xiaohongshu.UserProfileResponse

	err = withBrowserPage(ctx, func(page *rod.Page) error {
		action := xiaohongshu.NewUserProfileAction(page)
		result, err = action.GetMyProfileViaSidebar(ctx, parsed)
		return err
	})

	if err != nil {
		return nil, err
	}

	response := &UserProfileResponse{
		UserBasicInfo: result.UserBasicInfo,
		Interactions:  result.Interactions,
		Feeds:         result.Feeds,
	}

	return response, nil
}
