package xiaohongshu

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/errors"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

type SearchResult struct {
	Search struct {
		Feeds FeedsValue `json:"feeds"`
	} `json:"search"`
}

// FilterOption 筛选选项结构体
type FilterOption struct {
	SortBy      string `json:"sort_by,omitempty" jsonschema:"排序依据: 综合|最新|最多点赞|最多评论|最多收藏,默认为'综合'"`
	NoteType    string `json:"note_type,omitempty" jsonschema:"笔记类型: 不限|视频|图文,默认为'不限'"`
	PublishTime string `json:"publish_time,omitempty" jsonschema:"发布时间: 不限|一天内|一周内|半年内,默认为'不限'"`
	SearchScope string `json:"search_scope,omitempty" jsonschema:"搜索范围: 不限|已看过|未看过|已关注,默认为'不限'"`
	Location    string `json:"location,omitempty" jsonschema:"位置距离: 不限|同城|附近,默认为'不限'"`
}

// filterGroup 面板上的一个筛选组：标签是什么、对应入参的哪个字段、允许哪些取值。
//
// 组和选项一律按文本定位，不用序号。面板里同一个选项可能渲染成多个 div.tags
// （数量随视口而变），首项是否重复各组也不一致，下标对不齐。
type filterGroup struct {
	label   string                    // 面板上这一组的标签文本
	pick    func(FilterOption) string // 从入参里取这一组的值
	allowed []string                  // 合法取值；在打开页面之前就能挡掉写错的值
}

var filterGroups = []filterGroup{
	{"排序依据", func(f FilterOption) string { return f.SortBy },
		[]string{"综合", "最新", "最多点赞", "最多评论", "最多收藏"}},
	{"笔记类型", func(f FilterOption) string { return f.NoteType },
		[]string{"不限", "视频", "图文"}},
	{"发布时间", func(f FilterOption) string { return f.PublishTime },
		[]string{"不限", "一天内", "一周内", "半年内"}},
	{"搜索范围", func(f FilterOption) string { return f.SearchScope },
		[]string{"不限", "已看过", "未看过", "已关注"}},
	{"位置距离", func(f FilterOption) string { return f.Location },
		[]string{"不限", "同城", "附近"}},
}

// pendingFilter 一个待应用的筛选项。
type pendingFilter struct {
	group  string // 组标签
	option string // 选项文本
}

// splitNoteTypeFilters 将已经移出筛选面板的「笔记类型」单独交给顶部标签处理。
func splitNoteTypeFilters(pending []pendingFilter) (noteType, panel []pendingFilter) {
	for _, pf := range pending {
		if pf.group == "笔记类型" {
			noteType = append(noteType, pf)
			continue
		}
		panel = append(panel, pf)
	}
	return noteType, panel
}

// collectFilters 把入参展开成待应用的筛选项，顺便校验取值。
//
// 校验放在这里是为了在打开浏览器之前就挡掉写错的值——否则要等导航、悬停、
// 在面板里找不到之后才能报错，等于为了说一句"你写错了"先向平台发一次请求。
func collectFilters(filters []FilterOption) ([]pendingFilter, error) {
	var pending []pendingFilter

	for _, f := range filters {
		for _, g := range filterGroups {
			value := g.pick(f)
			if value == "" {
				continue
			}
			if !slices.Contains(g.allowed, value) {
				return nil, fmt.Errorf("%s 不支持 %q，可选：%s",
					g.label, value, strings.Join(g.allowed, "、"))
			}
			pending = append(pending, pendingFilter{group: g.label, option: value})
		}
	}

	return pending, nil
}

// pageTimeout 一次搜索的页面总预算。
const pageTimeout = 60 * time.Second

// probeTimeout 单个等待步骤的独立预算：等不到只花掉这些，不吃掉整个 pageTimeout。
const probeTimeout = 10 * time.Second

type SearchAction struct {
	page *rod.Page
}

func NewSearchAction(page *rod.Page) *SearchAction {
	pp := page.Timeout(pageTimeout)

	return &SearchAction{page: pp}
}

func (s *SearchAction) Search(ctx context.Context, keyword string, filters ...FilterOption) ([]Feed, error) {
	// 先校验筛选取值，必须在导航之前——写错的值不该先向平台发一次请求再报错。
	pending, err := collectFilters(filters)
	if err != nil {
		return nil, err
	}

	// 注意 .Context(ctx) 会替换掉 NewSearchAction 里设的 deadline，必须在其后重新 Timeout，
	// 否则搜索页不 stable 时等待会永久挂起（无 deadline 可依赖）。
	page := s.page.Context(ctx).Timeout(pageTimeout)

	// 搜索结果同样来自 __INITIAL_STATE__，封面图不必真的下载解码。
	defer blockHeavyResources(page)()

	searchURL := makeSearchURL(keyword)

	// rod 的 Navigate 发完 Page.navigate 就返回，不等重定向落地，紧接着读到的 URL
	// 还是跳转前的地址，风控判定必然落空。等 DOMContentLoaded，那之后才是 302 后的
	// 最终地址（实测 11ms 到；lifecycle 的 commit 事件 Chrome 不发，等它只会白等满预算）。
	waitLanded := page.Timeout(probeTimeout).WaitNavigation(proto.PageLifecycleEventNameDOMContentLoaded)
	page.MustNavigate(searchURL)
	waitLanded()

	if err := checkRiskVerification(page); err != nil {
		return nil, err
	}

	// 这里不再插一步 WaitStable：信息流页面有心跳/懒加载信标，请求永远不会空闲，
	// 它必然等满自己那份预算（实测 10s）才收场，而它挡在识别与读取之前——晚到的
	// 跳转要多花 10s 才被发现（实测 10.017s vs 354ms），正常页面也白等 10s。
	// 晚到的跳转由 waitSearchFeeds 循环里的 checkRiskVerification 接住，注水就绪由
	// 同一循环的 readSearchFeeds 接住，筛选那一步本来就有自己的元素等待。
	result, err := waitSearchFeeds(page, searchURL, 8*time.Second)
	if err != nil {
		return nil, err
	}

	humanize.Delay(ctx, humanize.AfterNavigate)

	if len(pending) > 0 {
		// 搜索页已将「笔记类型」从 hover 筛选面板移到顶部标签（全部 / 图文 / 视频）。
		// 先走标签，剩余条件仍在面板里选择；否则 note_type 会在面板中报「没有笔记类型组」。
		noteTypeFilters, panelFilters := splitNoteTypeFilters(pending)
		for _, pf := range noteTypeFilters {
			before := readFeedIDs(page)
			option, err := findNoteTypeTab(page, pf.option)
			if err != nil {
				return nil, err
			}

			humanize.Delay(ctx, humanize.BeforeClick)
			if err := humanize.ClickNoWait(option); err != nil {
				return nil, fmt.Errorf("点击笔记类型「%s」失败: %w", pf.option, err)
			}
			waitFeedsChanged(page, before, 15*time.Second)
		}

		if len(panelFilters) == 0 {
			if result, err = readSearchFeeds(page); err != nil {
				return nil, err
			}
		} else {
			// 先用带独立预算的克隆探一下筛选按钮：首屏正常、humanize.Delay 期间才被跳到
			// 验证页时，裸 MustElement 会一路等到页面 deadline 才 panic。
			if _, err := page.Timeout(probeTimeout).Element(`div.filter`); err != nil {
				return nil, riskOrError(page, "未找到筛选按钮 div.filter", err)
			}

			// 确认存在后从主 page 上重新取一次：从 Timeout 克隆上取到的元素连同它的 Mouse
			// 只剩探测那点预算，后面的悬停和点击会跟着一起短命。
			filterButton, err := page.Element(`div.filter`)
			if err != nil {
				return nil, riskOrError(page, "读取筛选按钮 div.filter 失败", err)
			}

			// 悬停在筛选按钮上展开面板
			if err := humanize.Hover(filterButton); err != nil {
				return nil, fmt.Errorf("悬停筛选按钮失败: %w", err)
			}
			humanize.Delay(ctx, humanize.BeforeClick)

			// 等待筛选面板出现，同样给独立预算
			if err := page.Timeout(probeTimeout).Wait(rod.Eval(filterPanelJS)); err != nil {
				return nil, riskOrError(page, "筛选面板未出现", err)
			}

			// 记下筛选前的结果，用来判断筛选后的数据什么时候到位。上面的注水轮询保证了
			// before 是一批真实的 id：waitFeedsChanged 的退出条件是 now != "" && now != before，
			// before 为空时第一次读到非空就返回，等于没等筛选生效。
			before := readFeedIDs(page)

			// 用 ClickNoWait：筛选面板是 hover 浮层，rod 的 WaitInteractable 会误判被遮挡而死等；
			// ClickNoWait 移进面板内选项（维持 hover、面板不关）再点。
			for _, pf := range panelFilters {
				option, err := findFilterOption(page, pf)
				if err != nil {
					return nil, err
				}
				humanize.Delay(ctx, humanize.BeforeClick)
				if err := humanize.ClickNoWait(option); err != nil {
					return nil, fmt.Errorf("点击筛选选项「%s」失败: %w", pf.option, err)
				}
			}

			waitFeedsChanged(page, before, 15*time.Second)

			// 筛选后是另一批数据，首屏那次读到的作废
			if result, err = readSearchFeeds(page); err != nil {
				return nil, err
			}
		}
	}

	if result == "" {
		// 兜底再判一次：也可能是点筛选、等刷新这十几秒里才被跳到验证页的。
		if err := checkRiskVerification(page); err != nil {
			return nil, err
		}
		return nil, noFeedsError(page, searchURL, "搜索结束时页面上没有 feeds 数据")
	}

	var feeds []Feed
	if err := json.Unmarshal([]byte(result), &feeds); err != nil {
		return nil, fmt.Errorf("failed to unmarshal feeds: %w", err)
	}

	notes := onlyNotes(feeds)
	logSearchYield(page, keyword, len(feeds), len(notes))

	return notes, nil
}

// logSearchYield 记一行「原始条数 / 笔记条数」。
//
// 空结果是软性风控最常见的样子：验证墙撤掉了、页面正常打开、__INITIAL_STATE__ 也
// 注水了，就是一条笔记都不给。这时返回的 count=0 跟「这个词真的没内容」在客户端看
// 来一模一样，日志里也查不出区别。实测过一次：账号解除硬验证之后，「咖啡」「深圳美食」
// 这类大词连续返回 0 条，而页面正文只剩导航栏和备案号。
//
// 两个数分开记才说得清是哪一种：原始就是 0 → 站点没给数据；原始有、笔记为 0 →
// 只回了广告和用户卡片，被 onlyNotes 滤光了。后者更像定向限流，所以抬到 Warn 并
// 附上页面现场。
func logSearchYield(page *rod.Page, keyword string, raw, notes int) {
	if raw > 0 && notes == 0 {
		warnPageState(page, fmt.Sprintf("搜索 %q 拿到 %d 条结果但没有一条是笔记（疑似只回了广告/用户卡片）", keyword, raw))

		return
	}

	if raw == 0 {
		logrus.Warnf("搜索 %q 没有拿到任何结果（页面正常但 feeds 为空，可能是软性限流，也可能这个词确实没内容）", keyword)

		return
	}

	logrus.Infof("搜索 %q 命中 %d/%d 条笔记", keyword, notes, raw)
}

// filterPanelJS 筛选面板是否已经展开。
const filterPanelJS = `() => document.querySelector('div.filter-panel') !== null`

// noteTypeTabText 将接口参数映射到搜索页顶部的内容标签。
var noteTypeTabText = map[string]string{
	"不限": "全部",
	"视频": "视频",
	"图文": "图文",
}

// noteTypeTabJS 在筛选按钮附近的标签组中找「全部 / 图文 / 视频」。限定为包含筛选按钮
// 的最小父容器，避免点到左侧频道栏或搜索结果正文中的同名文本。
const noteTypeTabJS = `(wanted => {
	const filter = document.querySelector('div.filter');
	if (!filter) return null;

	const labels = new Set(['全部', '图文', '视频']);
	const textOf = el => (el.textContent || '').trim();
	const visible = el => {
		const style = window.getComputedStyle(el);
		const rect = el.getBoundingClientRect();
		return style.display !== 'none' && style.visibility !== 'hidden' && rect.width > 0 && rect.height > 0;
	};

	for (let scope = filter.parentElement; scope && scope !== document.body; scope = scope.parentElement) {
		const items = [...scope.querySelectorAll('*')].filter(el =>
			el.children.length === 0 && labels.has(textOf(el)) && visible(el));
		if (new Set(items.map(textOf)).size < 3) continue;

		return items.find(el => textOf(el) === wanted) || null;
	}
	return null;
})`

func findNoteTypeTab(page *rod.Page, option string) (*rod.Element, error) {
	tabText, ok := noteTypeTabText[option]
	if !ok {
		return nil, fmt.Errorf("未知的笔记类型「%s」", option)
	}

	// 先用独立预算探测，避免验证页或改版页让主页面的查找一直等到总 deadline。
	// 真正点击时从主页面重取，不能带着探测副本仅剩的短 deadline。
	if _, err := page.Timeout(probeTimeout).ElementByJS(rod.Eval(noteTypeTabJS, tabText)); err != nil {
		return nil, riskOrError(page, fmt.Sprintf("未找到笔记类型顶部标签「%s」", tabText), err)
	}

	tab, err := page.ElementByJS(rod.Eval(noteTypeTabJS, tabText))
	if err != nil {
		return nil, riskOrError(page, fmt.Sprintf("未找到笔记类型顶部标签「%s」", tabText), err)
	}
	return tab, nil
}

// riskOrError 筛选路径上的失败收口：先看一眼是不是被风控拦了（首屏正常、等待期间
// 才被跳转是常见形态），不是就按原因报错。
func riskOrError(page *rod.Page, what string, cause error) error {
	if err := checkRiskVerification(page); err != nil {
		return err
	}

	// 不是安全验证，那就是一种没被识别出来的形态（登录墙、站点改版、真的慢）。
	// 不留痕的话客户端只拿到一句「未找到 xxx: context deadline exceeded」，
	// 日志里一行都没有，运维无从判断到底被带到哪了。
	warnPageState(page, what)

	return fmt.Errorf("%s: %w", what, cause)
}

// noFeedsError 报「没读到结果」，同时把页面现场打进日志。最终 URL 和请求的搜索页不
// 一致时（登录墙这类没被识别出来的拦截形态）把它拼进错误，客户端至少知道被带到哪了。
func noFeedsError(page *rod.Page, searchURL, reason string) error {
	finalURL := warnPageState(page, reason)
	if finalURL != "" && finalURL != searchURL {
		return fmt.Errorf("%w（最终停留在 %s）", errors.ErrNoFeeds, finalURL)
	}

	return errors.ErrNoFeeds
}

// searchFeedsJS 读搜索结果的 feeds 原始 JSON。
//
// 搜不到东西时返回 "[]" 而不是 ""，这两者必须分开——"" 只代表「还没注水」。
// 下面 feedIDsJS 的 join(",") 对空数组同样给 ""，拿它当就绪条件的话，一个真的
// 没有结果的关键词会白等满 8 秒再报 ErrNoFeeds，把「确实没有结果」这个正确答案
// 变成一次硬失败。所以两段 JS 不能互相替代。
const searchFeedsJS = `() => {
	const f = window.__INITIAL_STATE__?.search?.feeds;
	const v = f ? (f.value !== undefined ? f.value : f._value) : null;
	return v ? JSON.stringify(v) : "";
}`

// readSearchFeeds 读一次注水结果。
//
// 页面 ctx 过期时 Eval 会立刻返回 context 错误（不 panic），必须原样报出来：吞成空串
// 的话「页面超时」和「还没注水」在下游完全同形，只能一路空转到超时，再报一句无从
// 判断的「没有捕获到 feeds 数据」。
func readSearchFeeds(page *rod.Page) (string, error) {
	res, err := page.Eval(searchFeedsJS)
	if err == nil {
		return res.Value.Str(), nil
	}

	// pageInfo 走 browser 的 ctx，页面 deadline 烧穿之后依然读得到——这正是它唯一
	// 还能留下现场的场景。不留就只剩一句「页面加载超时」，连页面停在哪都不知道。
	switch ctxErr := page.GetContext().Err(); {
	case stderrors.Is(ctxErr, context.DeadlineExceeded):
		warnPageState(page, "读取搜索结果时页面已超时")
		return "", fmt.Errorf("页面加载超时（%s），未能读到搜索结果", pageTimeout)
	case ctxErr != nil:
		warnPageState(page, "读取搜索结果时请求已被取消")
		return "", fmt.Errorf("搜索已被取消，未能读到搜索结果: %w", ctxErr)
	default:
		return "", fmt.Errorf("读取搜索结果失败: %w", err)
	}
}

// waitSearchFeeds 等首屏搜索结果注水就绪，顺带识别风控拦截。
//
// 原先是 MustWait(__INITIAL_STATE__ !== undefined)：这个壳首屏就存在、立刻返回，等于
// 没等，后面读到的可能是还没灌数据的空 feeds——偶发的 ErrNoFeeds 就是这么来的。
// 改成 feeds.go 那套注水轮询：8s 预算、300ms 间隔、先读后睡，正常页面上不额外增加延迟。
func waitSearchFeeds(page *rod.Page, searchURL string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		result, err := readSearchFeeds(page)
		if err != nil {
			return "", err
		}
		if result != "" {
			return result, nil
		}
		if err := checkRiskVerification(page); err != nil {
			return "", err
		}
		if time.Now().After(deadline) {
			return "", noFeedsError(page, searchURL,
				fmt.Sprintf("首屏搜索结果在 %s 内没有注水就绪", timeout))
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// feedIDsJS 读当前结果集的 id 列表，用来判断数据有没有换一批。
const feedIDsJS = `() => {
	const f = window.__INITIAL_STATE__?.search?.feeds;
	const v = f ? (f.value !== undefined ? f.value : f._value) : null;
	return v ? v.map(x => x.id).join(",") : "";
}`

func readFeedIDs(page *rod.Page) string {
	res, err := page.Eval(feedIDsJS)
	if err != nil {
		return ""
	}
	return res.Value.Str()
}

// waitFeedsChanged 等筛选后的数据到位。
//
// 点完筛选项不能立刻读结果：站点是先把 feeds 清空、再灌入新数据，中间这段时间读到的
// 要么是空，要么还是筛选前那一批。
//
// 超时不报错：筛选已经点上了，宁可返回可能偏旧的数据，也不要整个搜索失败。
func waitFeedsChanged(page *rod.Page, before string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if now := readFeedIDs(page); now != "" && now != before {
			return
		}
		if page.GetContext().Err() != nil {
			// 页面 ctx 已经结束，再轮询也读不到，错误交给随后的 readSearchFeeds 收口
			return
		}
		time.Sleep(300 * time.Millisecond)
	}

	logrus.Warnf("筛选后结果集未发生变化（%s），可能是该筛选项本就不改变结果", timeout)
}

// findFilterOption 在筛选面板里定位一个选项：按标签找到组，再在组内按文本找选项。
//
// 全程不用序号。同一个选项在面板里可能渲染成多个 div.tags（数量随视口而变，
// 且首项是否重复各组不一致），下标对不齐；早前用 div.tags:nth-child(N) 会选错项。
// 多份重复的位置尺寸完全相同，取第一个点下去落在同一处。
//
// 作用域必须限定在 div.filter-panel 内且只认 div.tags：页面别处存在同文本的
// 可见元素（顶部频道栏的「图文」「视频」、标签「综合」），放宽会点错地方。
func findFilterOption(page *rod.Page, pf pendingFilter) (*rod.Element, error) {
	groups, err := page.Elements("div.filter-panel div.filters")
	if err != nil {
		return nil, fmt.Errorf("读取筛选面板失败: %w", err)
	}

	for _, group := range groups {
		// 组标签是 div.filters 下的直接子 span
		label, err := group.Element(":scope > span")
		if err != nil {
			continue
		}
		text, err := label.Text()
		if err != nil || strings.TrimSpace(text) != pf.group {
			continue
		}

		options, err := group.Elements("div.tags")
		if err != nil {
			return nil, fmt.Errorf("读取「%s」的选项失败: %w", pf.group, err)
		}

		var available []string
		for _, opt := range options {
			t, err := opt.Text()
			if err != nil {
				continue
			}
			t = strings.TrimSpace(t)
			if t == pf.option {
				return opt, nil
			}
			available = append(available, t)
		}
		return nil, fmt.Errorf("「%s」里没有选项「%s」，页面上是：%s",
			pf.group, pf.option, strings.Join(available, "、"))
	}

	return nil, fmt.Errorf("筛选面板里没有「%s」这一组", pf.group)
}

func makeSearchURL(keyword string) string {

	values := url.Values{}
	values.Set("keyword", keyword)
	values.Set("source", "web_explore_feed")

	//https://www.xiaohongshu.com/search_result?keyword=%25E7%258E%258B%25E5%25AD%2590&source=web_search_result_notes
	//https://www.xiaohongshu.com/search_result?keyword=%25E7%258E%258B%25E5%25AD%2590&source=web_explore_feed
	return fmt.Sprintf("https://www.xiaohongshu.com/search_result?%s", values.Encode())
}
