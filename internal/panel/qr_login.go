// qr_login.go 授权二维码生成。
//
// 设备码 / 设备授权流程本身会返回一个可在手机或另一台浏览器打开的授权 URL，
// 把该 URL 编成二维码即可实现「扫码登录」：手机扫码完成登录，面板按 state
// 轮询令牌，自动把 Token / Cookie / 账号标识写入号池，不需要读本机客户端。
package panel

import (
	"encoding/base64"
	"fmt"

	qrcode "github.com/skip2/go-qrcode"
)

const (
	qrImageSize     = 320
	qrMaxURLLen     = 2048
	qrDataURIPrefix = "data:image/png;base64,"
)

// qrDataURL 把授权链接编码为可直接放进 <img src> 的 PNG data URI。
// 面板 CSP 允许 img-src 'self' data:，因此无需额外静态资源或外部 QR 服务。
func qrDataURL(rawURL string) (string, error) {
	if rawURL == "" {
		return "", fmt.Errorf("empty authorization url")
	}
	if len(rawURL) > qrMaxURLLen {
		return "", fmt.Errorf("authorization url too long (%d > %d)", len(rawURL), qrMaxURLLen)
	}
	png, err := qrcode.Encode(rawURL, qrcode.Medium, qrImageSize)
	if err != nil {
		return "", err
	}
	return qrDataURIPrefix + base64.StdEncoding.EncodeToString(png), nil
}
