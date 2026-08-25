package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 二维码是 get_verification_qrcode 唯一的交付物：mime 拆错或把整条 data-URI
// 当 base64 塞进 image，客户端就只能渲染出一张碎图，用户没码可扫。
func TestSplitDataURI(t *testing.T) {
	t.Run("实测的验证码形态", func(t *testing.T) {
		mime, data, ok := splitDataURI("data:image/png;base64,iVBORw0KGgo=")
		require.True(t, ok)
		assert.Equal(t, "image/png", mime)
		assert.Equal(t, "iVBORw0KGgo=", data)
	})

	// 站点换 mime 时要跟着走，不能写死 png
	t.Run("其他 mime 原样取出", func(t *testing.T) {
		mime, _, ok := splitDataURI("data:image/webp;base64,UklGRg==")
		require.True(t, ok)
		assert.Equal(t, "image/webp", mime)
	})

	// 拆不出来必须明说，交给调用方降级成文本，而不是硬塞一段废数据
	t.Run("不是 base64 data-URI 就报不 ok", func(t *testing.T) {
		for _, src := range []string{
			"",
			"https://example.com/qrcode.png",
			"data:image/png,not-base64",
			"data:;base64,iVBORw0KGgo=",
			"data:image/png;base64,",
		} {
			_, _, ok := splitDataURI(src)
			assert.False(t, ok, "src=%q 不该被当成 base64 data-URI", src)
		}
	})
}
