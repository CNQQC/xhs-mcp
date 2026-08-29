package main

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// MCP 侧的短句柄表。
//
// 一次搜索 20 条，每条都要带 id（24 位十六进制）、xsecToken（40+ 字符）和作者
// userId（24 位十六进制），合计约占单条返回的三成。这些字节对模型没有任何阅读
// 价值——它只是要在下一次调用里原样传回来。这里把它们换成一个几字符的 ref，
// 真正的句柄留在服务端。
//
// ref 带一段每进程随机的前缀，这是安全要求而不是美观要求：序号从 1 重新开始，
// 而模型手上可能还攥着重启前的 ref，没有前缀的话旧 ref 会稳稳落到另一条笔记上，
// 于是点赞、收藏、评论全都作用到别人的帖子上。带上前缀，重启后旧 ref 一律查不到，
// 报错让模型重搜——宁可让它多搜一次，也不能张冠李戴。
const (
	// refTTL 一条 ref 的有效期。够长到模型在一轮对话里反复引用，
	// 又不至于让表无限涨。
	refTTL = time.Hour

	// refMaxEntries 表的容量上限。到顶了先清过期的，还不够就按插入顺序丢最老的。
	// 一次搜索最多 20 来条，这个数够几百次搜索。
	refMaxEntries = 4096
)

// refTarget 一个 ref 背后的真实句柄。
//
// 笔记 ref 有 FeedID/XsecToken/UserID（作者）；评论 ref 另外还有 CommentID 和
// 评论者的 UserID。工具要什么就从这里取什么。
type refTarget struct {
	FeedID    string
	XsecToken string
	UserID    string
	CommentID string
}

type refEntry struct {
	target    refTarget
	seq       uint64
	expiresAt time.Time
}

// refTable 进程内的 ref 登记表。
type refTable struct {
	mu    sync.Mutex
	tag   string // 每进程随机前缀
	seq   uint64
	items map[string]refEntry
}

func newRefTable() *refTable {
	return &refTable{tag: newRefTag(), items: make(map[string]refEntry)}
}

// newRefTag 生成每进程前缀。
//
// 取不到随机数就退回时间戳：宁可前缀弱一点，也不能让服务起不来——而即便退化，
// 它仍然满足唯一要紧的性质：重启之后和上一轮不一样。
func newRefTag() string {
	buf := make([]byte, 5)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano()%100000)
	}

	// base32 无填充、全小写，只有字母和数字，不会在 JSON 或 URL 里需要转义
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))[:4]
}

// put 登记一个句柄，返回短 ref。
func (t *refTable) put(target refTarget) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.seq++
	ref := fmt.Sprintf("%s%d", t.tag, t.seq)
	t.items[ref] = refEntry{target: target, seq: t.seq, expiresAt: time.Now().Add(refTTL)}

	if len(t.items) > refMaxEntries {
		t.evictLocked()
	}

	return ref
}

// lookup 查一个 ref。查不到、过期，一律 false——上层给同一句「重新搜索」。
func (t *refTable) lookup(ref string) (refTarget, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	item, ok := t.items[ref]
	if !ok {
		return refTarget{}, false
	}
	if time.Now().After(item.expiresAt) {
		delete(t.items, ref)
		return refTarget{}, false
	}

	return item.target, true
}

// evictLocked 先清过期的，还超容量就按登记顺序丢最老的。
//
// 丢最老的而不是随机丢：模型引用的总是刚拿到的那一批，最老的那些本来也快没人问了。
func (t *refTable) evictLocked() {
	now := time.Now()
	for ref, item := range t.items {
		if now.After(item.expiresAt) {
			delete(t.items, ref)
		}
	}

	over := len(t.items) - refMaxEntries
	if over <= 0 {
		return
	}

	type aged struct {
		ref string
		seq uint64
	}
	all := make([]aged, 0, len(t.items))
	for ref, item := range t.items {
		all = append(all, aged{ref, item.seq})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seq < all[j].seq })

	for i := 0; i < over; i++ {
		delete(t.items, all[i].ref)
	}
}

// errRefExpired ref 查不到时对模型说的话。
//
// 要说清楚「重新搜一次」这个动作，否则模型会拿着同一个 ref 反复重试。
const errRefExpired = "ref 无效或已过期（服务重启后旧 ref 全部失效）。请重新调用 " +
	"search_feeds / list_feeds / get_feed_detail 取一份新的列表，用里面新的 ref；" +
	"也可以绕开 ref，直接传原始的 id 和 xsec_token。"

// resolveFeed 把 ref 还原成笔记句柄。
//
// 没给 ref 就用调用方直接传的 feed_id/xsec_token：这条老路一直留着——REST 侧本来
// 就只有原始句柄，而模型偶尔也会从别处（比如通知列表）直接拿到 id 和 token。
func (s *AppServer) resolveFeed(ref, feedID, xsecToken string) (refTarget, bool) {
	if ref != "" {
		return s.refs.lookup(ref)
	}
	if feedID != "" && xsecToken != "" {
		return refTarget{FeedID: feedID, XsecToken: xsecToken}, true
	}

	return refTarget{}, false
}

// resolveUser 把 ref 还原成用户句柄。
//
// 笔记 ref 和评论 ref 都带着对应的 UserID（作者 / 评论者），所以看主页不用另给一个
// ref：拿着那条笔记的 ref 就能翻到作者主页。
func (s *AppServer) resolveUser(ref, userID, xsecToken string) (refTarget, bool) {
	if ref != "" {
		target, ok := s.refs.lookup(ref)
		if !ok || target.UserID == "" {
			return refTarget{}, false
		}

		return target, true
	}
	if userID != "" && xsecToken != "" {
		return refTarget{UserID: userID, XsecToken: xsecToken}, true
	}

	return refTarget{}, false
}

// refError ref 查不到时给模型的回答。
func refError() *MCPToolResult {
	return &MCPToolResult{
		Content: []MCPContent{{Type: "text", Text: errRefExpired}},
		IsError: true,
	}
}
