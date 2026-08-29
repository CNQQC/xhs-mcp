package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefTableRoundTrip(t *testing.T) {
	tbl := newRefTable()

	ref := tbl.put(refTarget{FeedID: "f1", XsecToken: "tok1", UserID: "u1"})
	require.NotEmpty(t, ref)

	got, ok := tbl.lookup(ref)
	require.True(t, ok)
	assert.Equal(t, "f1", got.FeedID)
	assert.Equal(t, "tok1", got.XsecToken)
	assert.Equal(t, "u1", got.UserID)
}

// TestRefIsShort ref 得比它替代的东西短得多，否则这套机制就白做了。
func TestRefIsShort(t *testing.T) {
	tbl := newRefTable()

	ref := tbl.put(refTarget{FeedID: "65f1a2b3c4d5e6f7a8b9c0d1", XsecToken: "ABQpXKvL3nR8mYt2wZ7hJ4dF6sQ9bN1cE5gA0uI3oP"})

	assert.Less(t, len(ref), 12, "ref 应该只有几个字符，实际是 %q", ref)
}

// TestRefNotReusedAcrossRestart 这条是安全性质，不是美观问题。
//
// 序号在新进程里从 1 重来，而模型手上可能还攥着重启前的 ref。没有每进程前缀的话，
// 旧 ref 会稳稳落到另一条笔记上——于是点赞、收藏、评论全都作用到别人的帖子上。
func TestRefNotReusedAcrossRestart(t *testing.T) {
	before := newRefTable()
	oldRef := before.put(refTarget{FeedID: "老笔记"})

	// 模拟重启：换一张全新的表，序号同样从头开始
	after := newRefTable()
	newRef := after.put(refTarget{FeedID: "新笔记"})

	assert.NotEqual(t, oldRef, newRef, "重启前后的第一个 ref 不能撞上")

	_, ok := after.lookup(oldRef)
	assert.False(t, ok, "重启前的 ref 必须查不到，绝不能解析到新笔记上")
}

// TestRefExpires 过期的 ref 查不到。
func TestRefExpires(t *testing.T) {
	tbl := newRefTable()
	ref := tbl.put(refTarget{FeedID: "f1"})

	// 直接把到期时间拨到过去，免得测试真的等一小时
	tbl.mu.Lock()
	item := tbl.items[ref]
	item.expiresAt = time.Now().Add(-time.Second)
	tbl.items[ref] = item
	tbl.mu.Unlock()

	_, ok := tbl.lookup(ref)
	assert.False(t, ok)

	tbl.mu.Lock()
	_, still := tbl.items[ref]
	tbl.mu.Unlock()
	assert.False(t, still, "查到过期条目时应顺手删掉，别让表只进不出")
}

// TestRefEviction 表满了要丢最老的，而且不能把新的丢掉。
func TestRefEviction(t *testing.T) {
	tbl := newRefTable()

	first := tbl.put(refTarget{FeedID: "最老的"})
	for i := 0; i < refMaxEntries; i++ {
		tbl.put(refTarget{FeedID: "填充"})
	}
	last := tbl.put(refTarget{FeedID: "最新的"})

	tbl.mu.Lock()
	size := len(tbl.items)
	tbl.mu.Unlock()
	assert.LessOrEqual(t, size, refMaxEntries, "表不能无限涨")

	_, ok := tbl.lookup(first)
	assert.False(t, ok, "最老的应该先被丢掉")

	_, ok = tbl.lookup(last)
	assert.True(t, ok, "刚登记的绝不能被丢掉——模型引用的就是它")
}

// TestResolveFallback 没走 ref 的老路要一直留着：REST 侧只有原始句柄，
// 模型也可能从通知列表那种地方直接拿到 id 和 token。
func TestResolveFallback(t *testing.T) {
	app := &AppServer{refs: newRefTable()}

	t.Run("直接传 id+token", func(t *testing.T) {
		got, ok := app.resolveFeed("", "f1", "tok1")
		require.True(t, ok)
		assert.Equal(t, "f1", got.FeedID)
		assert.Equal(t, "tok1", got.XsecToken)
	})

	t.Run("ref 优先于 id+token", func(t *testing.T) {
		ref := app.refs.put(refTarget{FeedID: "来自ref", XsecToken: "tok-ref"})

		got, ok := app.resolveFeed(ref, "f1", "tok1")
		require.True(t, ok)
		assert.Equal(t, "来自ref", got.FeedID)
	})

	t.Run("查不到的 ref 直接失败，不悄悄回退", func(t *testing.T) {
		_, ok := app.resolveFeed("查无此ref", "f1", "tok1")
		assert.False(t, ok, "ref 给错了要报错，不能悄悄用另一条笔记顶上")
	})

	t.Run("两样都没给", func(t *testing.T) {
		_, ok := app.resolveFeed("", "", "")
		assert.False(t, ok)
	})

	t.Run("只给一半也不行", func(t *testing.T) {
		_, ok := app.resolveFeed("", "f1", "")
		assert.False(t, ok, "缺 token 调不动接口，不如当场说清楚")
	})
}

// TestResolveUser 笔记 ref 能翻到作者主页；没带 UserID 的 ref 不行。
func TestResolveUser(t *testing.T) {
	app := &AppServer{refs: newRefTable()}

	withUser := app.refs.put(refTarget{FeedID: "f1", XsecToken: "tok1", UserID: "u1"})
	got, ok := app.resolveUser(withUser, "", "")
	require.True(t, ok)
	assert.Equal(t, "u1", got.UserID)
	assert.Equal(t, "tok1", got.XsecToken)

	noUser := app.refs.put(refTarget{FeedID: "f2", XsecToken: "tok2"})
	_, ok = app.resolveUser(noUser, "", "")
	assert.False(t, ok, "这个 ref 上没有用户，不能拿它去查主页")
}
