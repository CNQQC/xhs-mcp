package errors

import "errors"

var ErrNoFeeds = errors.New("没有捕获到 feeds 数据")
var ErrNoFeedDetail = errors.New("没有捕获到 feed 详情数据")
var ErrRiskVerification = errors.New("被小红书安全验证拦截")
