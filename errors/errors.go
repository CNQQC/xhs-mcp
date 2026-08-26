package errors

import "errors"

var ErrNoFeeds = errors.New("没有捕获到 feeds 数据")
var ErrNoFeedDetail = errors.New("没有捕获到 feed 详情数据")
var ErrRiskVerification = errors.New("被小红书安全验证拦截")

// ErrVerifyThrottled 小红书对安全验证页本身限流：页面标题还是「安全验证」，
// 但正文换成了「请求太频繁，请一分钟后再试」，上面根本没有二维码可读。
//
// 单独立一个哨兵是因为上层要区别对待：这不是站点改版、也不是不支持的验证形态，
// 而是一分钟就能自愈的临时状态——验证网页该等一会儿自动重试，而不是把用户手上
// 那条 15 分钟有效的链接就此判死。
var ErrVerifyThrottled = errors.New("小红书对验证页限流")
