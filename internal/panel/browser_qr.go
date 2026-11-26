// browser_qr.go 容器内无头浏览器扫码登录：把登录页自己渲染的二维码截图给面板。
//
// 设备码流程（主号池 / MiniMax）用的是上游签发的授权 URL，可以直接生成二维码；
// Trae / Qoder / 豆包 / CodeArts 的二维码由网页前端绘制，面板无法凭 URL 复现，
// 因此改为：启动隔离浏览器 → 打开登录页 → CDP 截取二维码元素 → 用户手机扫码 →
// 沿用各平台既有 browser/poll 接口读取浏览器会话、写入号池。
package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// browserQRFindJS 在页面（含同源 iframe）里找可见的二维码元素，返回裁剪框。
// 找不到时返回 null，调用方退化为截取整个视口，避免因为选择器变化直接失败。
const browserQRFindJS = `(() => {
  const docs = [document];
  try {
    for (const f of document.querySelectorAll('iframe')) {
      if (f.contentDocument) docs.push(f.contentDocument);
    }
  } catch (e) {}
  const selectors = [
    'img[src*="qr" i]',
    'svg[class*="qr" i]',
    '[class*="qrcode" i]', '[id*="qrcode" i]',
    '[class*="qr-code" i]', '[id*="qr_code" i]',
    '[class*="scan" i]', '[id*="scan" i]',
    'canvas', 'img', 'svg'
  ];
  for (const doc of docs) {
    const seen = new Set();
    for (const sel of selectors) {
      let els;
      try { els = doc.querySelectorAll(sel); } catch (e) { continue; }
      for (const el of els) {
        if (seen.has(el)) continue;
        seen.add(el);
        let r;
        try { r = el.getBoundingClientRect(); } catch (e) { continue; }
        if (!r || r.width < 72 || r.height < 72 || r.width > 900 || r.height > 900) continue;
        if (r.width / r.height < 0.75 || r.width / r.height > 1.35) continue;
        if (r.bottom < 0 || r.right < 0 || r.top > innerHeight || r.left > innerWidth) continue;
        let st;
        try { st = doc.defaultView.getComputedStyle(el); } catch (e) { continue; }
        if (!st || st.display === 'none' || st.visibility === 'hidden' || st.opacity === '0') continue;
        return JSON.stringify({
          x: Math.max(0, r.left), y: Math.max(0, r.top),
          width: Math.min(r.width, innerWidth - Math.max(0, r.left)),
          height: Math.min(r.height, innerHeight - Math.max(0, r.top))
        });
      }
    }
  }
  return JSON.stringify(null);
})()`

// 有些平台首页只给一个「登录」入口，二维码藏在弹窗里。截图前点一次入口，
// 让页面进入真正的扫码状态；每个调试端口只做一次，避免轮询时反复点击。
const browserQRPrepareJS = `(() => {
  // 国内版 Qoder 走阿里云 SSO：底部第三方登录是支付宝图标，点它才出二维码。
  if (location.href.indexOf('account.aliyun.com/sso/login.htm') >= 0) {
    const alipay = document.querySelectorAll('[class*="alipay" i],[id*="alipay" i],[data-provider="alipay"],[aria-label*="支付宝"],[title*="支付宝"]');
    for (const el of alipay) { try { el.click(); return JSON.stringify(true); } catch (e) {} }
    for (const el of document.querySelectorAll('button,a,div,span,[role="button"]')) {
      const t = (el.textContent || '').trim();
      if (t === '支付宝' || t === '支付宝登录') { try { el.click(); return JSON.stringify(true); } catch (e) {} }
    }
    return JSON.stringify(false);
  }
  const close = document.querySelectorAll('[class*="close"],[class*="Close"],button,div[role="button"]');
  for (const el of close) {
    const t = (el.textContent || '').trim();
    if (t === '×' || t === '✕' || t === '关闭') { try { el.click(); } catch (e) {} break; }
  }
  const entries = document.querySelectorAll('button,a,[role="button"],[class*="login"]');
  for (const el of entries) {
    const t = (el.textContent || '').trim();
    if (t === '登录' || t === '扫码登录' || t === '使用网页端' || t.indexOf('登录华为云') >= 0) {
      try { el.click(); return JSON.stringify(true); } catch (e) {}
    }
  }
  return JSON.stringify(false);
})()`

var browserQRPrepared sync.Map // key=port:platform → time.Time

func browserQRPrepareOnce(port int, platform string, ws *wsConn) {
	key := fmt.Sprintf("%d:%s", port, platform)
	if last, ok := browserQRPrepared.Load(key); ok {
		if time.Since(last.(time.Time)) < 5*time.Minute {
			return
		}
	}
	// 页面可能还没加载完，第一次点不到入口：只有真的点到才记账，
	// 否则下一轮还要再试，不能让加载时序把扫码入口永远卡住。
	raw, err := caEval(ws, browserQRPrepareJS)
	var clicked bool
	if err == nil && json.Unmarshal(raw, &clicked) == nil && clicked {
		browserQRPrepared.Store(key, time.Now())
	}
}

type browserQRBox struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// browserQRPort 取当前平台登录浏览器的调试端口。复用各平台既有会话，
// 不新开浏览器、不改变登录状态，截图失败不影响原有登录链路。
func (p *Panel) browserQRPort(platform, email string) (int, error) {
	switch platform {
	case "trae":
		traeBrowserSession.mu.Lock()
		defer traeBrowserSession.mu.Unlock()
		if traeBrowserSession.port > 0 && traeBrowserSession.proc != nil {
			return traeBrowserSession.port, nil
		}
	case "doubao":
		doubaoBrowserSession.mu.Lock()
		defer doubaoBrowserSession.mu.Unlock()
		if doubaoBrowserSession.port > 0 && doubaoBrowserSession.proc != nil {
			return doubaoBrowserSession.port, nil
		}
	case "codearts":
		codeartsBrowserSession.mu.Lock()
		defer codeartsBrowserSession.mu.Unlock()
		if codeartsBrowserSession.port > 0 && codeartsBrowserSession.cmd != nil {
			return codeartsBrowserSession.port, nil
		}
	case "qoder":
		email = strings.TrimSpace(email)
		if email == "" {
			return 0, fmt.Errorf("missing email")
		}
		qoderWebSessions.mu.Lock()
		defer qoderWebSessions.mu.Unlock()
		if st := qoderWebSessions.by[email]; st != nil && st.port > 0 && st.proc != nil && !st.done {
			return st.port, nil
		}
	default:
		return 0, fmt.Errorf("unknown platform %q", platform)
	}
	return 0, fmt.Errorf("该平台的登录浏览器尚未启动或已结束，请重新发起登录")
}

// cdpQRScreenshot 在已有页面里定位二维码并截图，返回可直接放进 <img src> 的 data URI。
// cdpFindQRBoxDOM 用 CDP 的 DOM 域穿透 iframe / shadow DOM 找二维码元素。
// 网页 JS 只能读同源 iframe，登录页常把二维码放在第三方登录 iframe 里，
// 所以 DOM.getDocument(pierce=true) 是这里最可靠的兜底。
func cdpFindQRBoxDOM(ws *wsConn) *browserQRBox {
	// 按「像二维码的程度」排序：命名里带 qr / scan 的最优，其次 canvas 与图片。
	// 逐个 selector 查询而不是合并成一个，是为了避免 DOM 文档顺序把营销弹窗
	// 排在真二维码前面（实测豆包首页的 710x279 弹窗就会抢中）。
	selectors := []string{
		`img[src*="qr" i]`, `svg[class*="qr" i]`,
		`[class*="qrcode" i]`, `[id*="qrcode" i]`,
		`[class*="qr-code" i]`, `[id*="qr_code" i]`,
		`[class*="scan" i]`, `[id*="scan" i]`,
		`canvas`, `img`, `svg`,
	}
	raw, err := doubaoCDPCall(ws, 21, "DOM.getDocument", map[string]any{"depth": -1, "pierce": true}, 10*time.Second)
	if err != nil {
		return nil
	}
	var doc struct {
		Root struct {
			NodeID int `json:"nodeId"`
		} `json:"root"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Root.NodeID == 0 {
		return nil
	}
	for _, selector := range selectors {
		raw, err = doubaoCDPCall(ws, 22, "DOM.querySelectorAll", map[string]any{
			"nodeId": doc.Root.NodeID, "selector": selector,
		}, 10*time.Second)
		if err != nil {
			continue
		}
		var found struct {
			NodeIDs []int `json:"nodeIds"`
		}
		if json.Unmarshal(raw, &found) != nil {
			continue
		}
		for _, id := range found.NodeIDs {
			raw, err := doubaoCDPCall(ws, 23, "DOM.getBoxModel", map[string]any{"nodeId": id}, 10*time.Second)
			if err != nil {
				continue
			}
			var bm struct {
				Model struct {
					Content []float64 `json:"content"`
					Width   float64   `json:"width"`
					Height  float64   `json:"height"`
				} `json:"model"`
			}
			if json.Unmarshal(raw, &bm) != nil || len(bm.Model.Content) < 8 {
				continue
			}
			w, h := bm.Model.Width, bm.Model.Height
			// 二维码基本是正方形；这条过滤掉了弹窗、banner 这类宽扁元素。
			if w < 72 || h < 72 || w > 900 || h > 900 {
				continue
			}
			if w/h < 0.75 || w/h > 1.35 {
				continue
			}
			x, y := bm.Model.Content[0], bm.Model.Content[1]
			for i := 2; i < 8; i += 2 {
				if bm.Model.Content[i] < x {
					x = bm.Model.Content[i]
				}
				if bm.Model.Content[i+1] < y {
					y = bm.Model.Content[i+1]
				}
			}
			return &browserQRBox{X: x, Y: y, Width: w, Height: h}
		}
	}
	return nil
}

// cdpQRPickPage 按平台挑页面：支付宝扫码可能另开标签页，此时不能再截原始 SSO 页。
func cdpQRPickPage(port int, platform string) (*wsConn, string, error) {
	if platform != "qoder" {
		return caPickPage(port)
	}
	pages, err := caListPages(port)
	if err != nil {
		return nil, "", err
	}
	var alipay, aliyun, any *caPageInfo
	for i := range pages {
		p := &pages[i]
		switch {
		case strings.Contains(p.URL, "alipay"):
			if alipay == nil {
				alipay = p
			}
		case strings.Contains(p.URL, "aliyun"):
			if aliyun == nil {
				aliyun = p
			}
		default:
			if any == nil {
				any = p
			}
		}
	}
	pick := alipay
	if pick == nil {
		pick = aliyun
	}
	if pick == nil {
		pick = any
	}
	if pick == nil {
		return nil, "", fmt.Errorf("浏览器里没有可用的页面")
	}
	ws, err := wsDial(pick.WS, 6*time.Second)
	if err != nil {
		return nil, "", err
	}
	return ws, pick.URL, nil
}

func cdpQRScreenshot(port int, platform string) (string, error) {
	ws, pageURL, err := cdpQRPickPage(port, platform)
	if err != nil {
		return "", err
	}
	defer ws.Close()

	browserQRPrepareOnce(port, platform, ws)
	box := cdpFindQRBoxDOM(ws)
	if box == nil {
		if raw, eerr := caEval(ws, browserQRFindJS); eerr == nil {
			var found *browserQRBox
			if json.Unmarshal(raw, &found) == nil {
				box = found
			}
		}
	}
	params := map[string]any{
		"format":                "png",
		"fromSurface":           true,
		"captureBeyondViewport": false,
	}
	if box != nil && box.Width >= 72 && box.Height >= 72 {
		params["clip"] = map[string]any{
			"x": box.X, "y": box.Y, "width": box.Width, "height": box.Height, "scale": 1,
		}
	}
	raw, err := doubaoCDPCall(ws, 11, "Page.captureScreenshot", params, 20*time.Second)
	if err != nil {
		return "", fmt.Errorf("截图失败（%s）: %w", pageURL, err)
	}
	var shot struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &shot); err != nil || shot.Data == "" {
		return "", fmt.Errorf("截图返回为空（%s）", pageURL)
	}
	return "data:image/png;base64," + shot.Data, nil
}

// browserQR 返回当前登录浏览器里的二维码截图。未找到二维码元素时返回整屏截图，
// 前端会显示「等待页面渲染二维码」直到登录页真正出现。
func (p *Panel) browserQR(w http.ResponseWriter, r *http.Request) {
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	email := strings.TrimSpace(r.URL.Query().Get("email"))
	port, err := p.browserQRPort(platform, email)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	qr, err := cdpQRScreenshot(port, platform)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	out := map[string]any{"ok": true, "qr": qr}
	// CodeArts 的二维码状态由页面自己的 /authui/qrcode/status 接口给出，
	// 顺手带回去让前端显示「已扫描 / 已过期」等状态；其它平台没有等价接口。
	if platform == "codearts" {
		st := codeartsQRProbe(port)
		out["qr_status"] = st.Status
		out["qr_error"] = st.ErrorCode
	}
	writeJSON(w, http.StatusOK, out)
}
