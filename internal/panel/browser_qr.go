// browser_qr.go 容器内无头浏览器扫码登录：把登录页自己渲染的二维码截图给面板。
//
// 设备码流程（主号池 / MiniMax）用的是上游签发的授权 URL，可以直接生成二维码；
// Trae / Qoder / 豆包 / CodeArts 的二维码由网页前端绘制，面板无法凭 URL 复现，
// 因此改为：启动隔离浏览器 → 打开登录页 → CDP 截取二维码元素 → 用户手机扫码 →
// 沿用各平台既有 browser/poll 接口读取浏览器会话、写入号池。
package panel

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png" // 既要解码截图，也要把裁剪后的图重新编码
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// browserQRFindJS 在页面（含同源 iframe）里找可见的二维码元素，返回裁剪框。
// 找不到时返回 null——调用方会显示「等待渲染」并在下一轮重试，绝不能退化成
// 截取整个视口。这里只负责给出「候选裁剪框」，候选到底是不是二维码由
// qrShotLooksLikeQR 对截图内容做校验；不要再靠 src/class 去猜装饰图：
// 实测 Trae 的真二维码就是一张 325KB 的 data:image/svg+xml，而豆包礼盒插画
// 反倒是短 URL 远程图，按体积/内联与否猜只会两头误判。
const browserQRFindJS = `(() => {
  const docs = [document];
  try {
    for (const f of document.querySelectorAll('iframe')) {
      if (f.contentDocument) docs.push(f.contentDocument);
    }
  } catch (e) {}
  // 命名的二维码容器优先，宽泛的 img/svg/canvas 兜底（仍需内容校验）。
  const selectors = [
    '#J-qrcode', '[class*="qrcode" i]', '[id*="qrcode" i]',
    '[class*="qr-code" i]', '[id*="qr_code" i]',
    '[class*="scan" i]', '[id*="scan" i]',
    'canvas[class*="barcode" i]', 'img[src*="qr" i]', 'svg[class*="qr" i]',
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
        if (!r || r.width < 100 || r.height < 100 || r.width > 600 || r.height > 600) continue;
        if (r.width / r.height < 0.78 || r.width / r.height > 1.35) continue;
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
//
// Qoder 支付宝登录是三跳：qoder 登录页 → 阿里云 SSO → 支付宝扫码页。
// 每一跳都要点对按钮，且**必须从 qoder 自己的登录页起步** —— 直接把浏览器
// 开到阿里云 SSO 页时，回调会因为前端状态对不上而返回 Bad Request，
// 一个会话 Cookie 都拿不到（2026-11-27 实测）。
const browserQRPrepareJS = `(() => {
  const href = location.href;
  // 支付宝扫码页本身就是二维码：页面上的「扫码登录 / 账密登录」是切换标签，
  // 点错会把二维码切走，这里一律不点。
  if (href.indexOf('alipay.com') >= 0) return JSON.stringify(false);
  // 阿里云 SSO：第三方登录入口是底部的支付宝图标按钮。
  if (href.indexOf('account.aliyun.com/sso/login.htm') >= 0) {
    const alipay = document.querySelectorAll('.sso-third-party-icon-btn[title*="支付宝"],.sso-third-party-icon-btn[aria-label*="支付宝"],[class*="alipay" i],[id*="alipay" i],[data-provider="alipay"],[aria-label*="支付宝"],[title*="支付宝"]');
    for (const el of alipay) { try { el.click(); return JSON.stringify(true); } catch (e) {} }
    for (const el of document.querySelectorAll('button,a,div,span,[role="button"]')) {
      const t = (el.textContent || '').trim();
      if (t === '支付宝' || t === '支付宝登录') { try { el.click(); return JSON.stringify(true); } catch (e) {} }
    }
    return JSON.stringify(false);
  }
  // Qoder 国内站登录页：点「使用阿里云登录」，让站点自己发起 SSO。
  if (href.indexOf('qoder.com.cn') >= 0) {
    for (const el of document.querySelectorAll('a,button,[role="button"]')) {
      const t = (el.textContent || '').trim();
      if (t.indexOf('阿里云登录') >= 0 || t.indexOf('使用阿里云') >= 0) {
        try { el.click(); return JSON.stringify(true); } catch (e) {}
      }
    }
    for (const el of document.querySelectorAll('a,button,[role="button"]')) {
      const t = (el.textContent || '').trim();
      if (t === '登录' || t === '扫码登录' || t === 'Sign in') { try { el.click(); return JSON.stringify(true); } catch (e) {} }
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

// browserQRDismissJS 隐藏盖在登录弹窗上层的营销弹窗 / 纯遮罩。
//
// 实测豆包点「登录」后会同时弹出「下载电脑版 新用户领 30 天订阅权益」营销弹窗，
// 它是全屏 div.fixed + 半透明 backdrop-blur 遮罩，把登录弹窗的二维码完全挡住：
// 二维码 DOM（div[data-testid=qrcode_image] > svg）明明在，CDP 按它的坐标裁剪
// 出来的却是营销弹窗文字，内容校验自然一直不通过。
//
// 只做 display:none，不点击任何关闭按钮——实测点 × 会把登录弹窗一起关掉。
// 含登录文案的容器一律跳过，避免误伤；这段 JS 每轮执行，因为营销弹窗可能
// 延迟出现，且对没有这类弹窗的平台是零副作用的空操作。
const browserQRDismissJS = `(() => {
  let n = 0;
  for (const e of document.querySelectorAll('div')) {
    const c = (e.className || '').toString();
    if (c.indexOf('fixed') < 0) continue;
    const t = (e.innerText || '').trim();
    if (/手机号登录|扫码登录|密码登录|验证码登录|账密登录/.test(t)) continue;
    const promo = t.indexOf('订阅权益') >= 0 || t.indexOf('立即下载') >= 0;
    const mask = c.indexOf('inset-0') >= 0 && t.length < 10;
    if (promo || mask) { e.style.display = 'none'; n++; }
  }
  return n;
})()`

// preparedQR 记录「这个调试端口上的登录页已经点过入口」，避免轮询时反复点。
//
// 必须连页面 URL 一起记：调试端口是复用的（freePort 分配完回收，下一次很容易
// 又给到同一个号），只看 port:platform 会让新拉起的浏览器继承上一次的「已准备」
// 标记 —— 于是不再点「支付宝」入口，页面停在 SSO 首页，二维码永远不出来。
// 这正是「第一次能拿到图，第二次拿不到」的成因之一。
type preparedQR struct {
	at  time.Time
	url string
}

var browserQRPrepared sync.Map // key=port:platform → preparedQR

// browserQRForget 在浏览器重新拉起时清掉该端口的准备标记。
func browserQRForget(port int, platform string) {
	browserQRPrepared.Delete(fmt.Sprintf("%d:%s", port, platform))
}

// qrURLKey 取 URL 的 scheme://host/path（忽略 query）：
// SSO 链接每次的 nonce/state 都不同，带 query 比较会让「同一页面」永远不相等。
func qrURLKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Scheme + "://" + u.Host + u.Path
}

func browserQRPrepareOnce(port int, platform string, ws *wsConn, pageURL string) {
	key := fmt.Sprintf("%d:%s", port, platform)
	if last, ok := browserQRPrepared.Load(key); ok {
		st := last.(preparedQR)
		if time.Since(st.at) < 5*time.Minute && qrURLKey(st.url) == qrURLKey(pageURL) {
			return
		}
	}
	// 页面可能还没加载完，第一次点不到入口：只有真的点到才记账，
	// 否则下一轮还要再试，不能让加载时序把扫码入口永远卡住。
	raw, err := caEval(ws, browserQRPrepareJS)
	var clicked bool
	if err == nil && json.Unmarshal(raw, &clicked) == nil && clicked {
		browserQRPrepared.Store(key, preparedQR{at: time.Now(), url: pageURL})
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
		// 支付宝扫码不带邮箱时用固定会话键（与 qoderWebStart 一致）。
		email = strings.TrimSpace(email)
		if email == "" {
			email = qoderSSOSessionKey
		}
		qoderWebSessions.mu.Lock()
		defer qoderWebSessions.mu.Unlock()
		if st := qoderWebSessions.by[email]; st != nil && st.port > 0 && st.proc != nil && !st.done {
			// 进程已被回收时端口一定不会再监听，直接报「需要重新发起」，
			// 比让前端一直等「二维码尚未渲染」有用得多。
			if qoderWebProcAlive(st.proc) && qoderWebPortAlive(st.port) {
				return st.port, nil
			}
			return 0, fmt.Errorf("登录浏览器已退出，请重新点「扫码登录（支付宝）」")
		}
	default:
		return 0, fmt.Errorf("unknown platform %q", platform)
	}
	return 0, fmt.Errorf("该平台的登录浏览器尚未启动或已结束，请重新发起登录")
}

// cdpQRScreenshot 在已有页面里定位二维码并截图，返回可直接放进 <img src> 的 data URI。

// cdpFindQRBoxesDOM 用 CDP 的 DOM 域穿透 iframe / shadow DOM，返回一组候选裁剪框。
// 网页 JS 只能读同源 iframe，登录页常把二维码放在第三方登录 iframe 里，
// 所以 DOM.getDocument(pierce=true) 是这里最可靠的兜底。
//
// 返回「多个」而不是一个：实测同一个登录页里往往并存好几个近正方形元素——
// 豆包有礼盒插画 + 二维码容器 + 二维码 svg，Qoder 支付宝页有 304×241 的外框、
// 190×185 的详情框和 120×120 的 canvas.barcode。只取第一个框时，一旦它恰好
// 是装饰图或尚未绘制的空白占位，整轮就白等了。交给调用方逐个截图校验更稳。
func cdpFindQRBoxesDOM(ws *wsConn) []browserQRBox {
	// 按「像二维码的程度」排序：命名里带 qr / scan / barcode 的最优，
	// 其次通用 canvas 与图片。逐个 selector 查询而不是合并，避免 DOM 文档
	// 顺序把营销弹窗排在真二维码前面。
	selectors := []string{
		`#J-qrcode`, `[class*="qrcode" i]`, `[id*="qrcode" i]`,
		`[class*="qr-code" i]`, `[id*="qr_code" i]`,
		`[class*="scan" i]`, `[id*="scan" i]`,
		`canvas[class*="barcode" i]`, `img[src*="qr" i]`, `svg[class*="qr" i]`,
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
	const maxBoxes = 8
	boxes := make([]browserQRBox, 0, maxBoxes)
	seen := map[string]bool{}
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
			// 只按尺寸筛「候选框」，是不是真二维码交给 qrShotLooksLikeQR
			// 对截图内容做校验。不要按 src/class 猜装饰图——Trae 的真二维码
			// 就是 325KB 的 data:image/svg+xml，按体积猜会把它误杀。
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
			if w < 100 || h < 100 || w > 900 || h > 900 {
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
			key := fmt.Sprintf("%.0f:%.0f:%.0f:%.0f", x, y, w, h)
			if seen[key] {
				continue
			}
			seen[key] = true
			boxes = append(boxes, browserQRBox{X: x, Y: y, Width: w, Height: h})
			if len(boxes) >= maxBoxes {
				return boxes
			}
		}
	}
	return boxes
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

// qrShotLooksLikeQR 用灰度统计判断截图到底是不是二维码。
//
// 靠 DOM 选择器 / 属性猜二维码并不可靠：实测豆包登录弹窗旁边有一张 171×164 的
// 礼盒插画（远程 URL 的 <img>，class 里既没有 logo/decor 也不是内联 SVG），旧逻辑
// 会把它当二维码截给用户；Qoder 支付宝二维码渲染前的首帧则是纯白空白图。
//
// 二维码的视觉特征很稳定：几乎只有纯黑/纯白，且水平/垂直方向高频黑白跳变；
// 装饰插画、空白帧、营销 banner 在「暗像素占比」和「跳变率」上都接近 0。
// 实测样本（64×64 采样）：
//
//	Trae 二维码      dark=0.36  bw=0.99  flip=0.38
//	CodeArts 二维码  dark=0.51  bw=1.00  flip=0.35
//	Qoder 二维码     dark=0.71  bw=0.87  flip=0.11
//	豆包礼盒插画     dark=0.06  bw=0.71  flip=0.005
//	整页截图         dark=0.03  bw=0.35  flip=0.020
//
// 阈值取 dark>=0.18 && bw>=0.75 && flip>=0.06，两边都留了足量余量。
func qrShotLooksLikeQR(png []byte) bool {
	img, _, err := image.Decode(bytes.NewReader(png))
	if err != nil {
		return false
	}
	b := img.Bounds()
	if b.Dx() < 8 || b.Dy() < 8 {
		return false
	}
	const n = 64
	g := make([]float64, n*n)
	for y := 0; y < n; y++ {
		sy := b.Min.Y + (y*b.Dy()+b.Dy()/2)/n
		for x := 0; x < n; x++ {
			sx := b.Min.X + (x*b.Dx()+b.Dx()/2)/n
			g[y*n+x] = float64(color.GrayModel.Convert(img.At(sx, sy)).(color.Gray).Y)
		}
	}
	dark, light := 0, 0
	for _, v := range g {
		if v < 90 {
			dark++
		} else if v > 170 {
			light++
		}
	}
	flip := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n-1; x++ {
			a, c := g[y*n+x], g[y*n+x+1]
			if (a < 90 && c > 170) || (a > 170 && c < 90) {
				flip++
			}
		}
	}
	for x := 0; x < n; x++ {
		for y := 0; y < n-1; y++ {
			a, c := g[y*n+x], g[(y+1)*n+x]
			if (a < 90 && c > 170) || (a > 170 && c < 90) {
				flip++
			}
		}
	}
	total := float64(n * n)
	flipRate := float64(flip) / float64(2*n*(n-1))
	return float64(dark)/total >= 0.18 &&
		float64(dark+light)/total >= 0.75 &&
		flipRate >= 0.06
}

// trimQRShot 把截图裁到二维码本体，返回重新编码的 PNG。
//
// 为什么需要：DOM 里的「二维码元素」常常是一个大卡片——上面有标题、右边有操作说明、
// 底部有「打开 App 扫码」提示（支付宝 / 阿里云 SSO 页面就是这样）。按元素框整块
// 截图虽然能通过内容校验（二维码够大），但用户看到的是「一张大图中间一个小码」，
// 在弹窗里被压缩后几乎扫不动。
//
// 做法是先做行列投影，而不是简单的暗像素包围盒：卡片标题、说明文字也是暗像素，
// 包围盒会被文字撑大。二维码的特征是**整行/整列都密集分布暗像素**（模块占 30%~50%），
// 文字行只有零星几个暗像素。所以：
//
//  1. 二值化后算每行的暗像素占比，取连续「≥0.18」的最长行区间 → 纵向范围；
//  2. 在该纵向范围内算每列占比，取连续「≥0.18」的最长列区间 → 横向范围；
//  3. 按 8% 留白（二维码静区）裁剪，比例异常时原样返回。
func trimQRShot(raw []byte) []byte {
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return raw
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < 60 || h < 60 {
		return raw
	}
	dark := make([]bool, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			g := color.GrayModel.Convert(src.At(b.Min.X+x, b.Min.Y+y)).(color.Gray).Y
			dark[y*w+x] = g < 110
		}
	}
	// 以「暗像素密度最高的那一行/列」为种子向两侧扩展，遇到连续空白带就断开。
	//
	// 为什么不用固定密度阈值：二维码在截图里可能只占整宽的 1/3，一行模块的暗像素
	// 占比本来就只有 0.1~0.2，卡阈值要么漏掉模块行、要么把标题文字一起收进来
	// （实测行密度在 0.21 上下波动，任何单点阈值都会把二维码切成几段）。
	// 从峰值行出发找「空白分隔带」则与绝对密度无关：卡片标题与二维码之间一定有
	// 一段空白，标题和说明文字自然被排除在外。
	extend := func(n int, rate func(int) float64, seed int, minGap int) (int, int) {
		const empty = 0.03
		lo, hi := seed, seed
		gap := 0
		for i := seed - 1; i >= 0; i-- {
			if rate(i) < empty {
				gap++
				if gap >= minGap {
					break
				}
				continue
			}
			gap = 0
			lo = i
		}
		gap = 0
		for i := seed + 1; i < n; i++ {
			if rate(i) < empty {
				gap++
				if gap >= minGap {
					break
				}
				continue
			}
			gap = 0
			hi = i
		}
		return lo, hi + 1
	}
	peak := func(n int, rate func(int) float64, hint int) int {
		// 种子不能取「密度最高的行」：卡片标题是一行实心文字，整行密度常常比
		// 只占 1/3 宽度的二维码模块行还高（实测 0.26 vs 0.21），会从标题开始裁。
		// 二维码的暗像素总量远多于文字，所以用暗像素质心当提示，取离它最近的
		// 「有内容」行 / 列 —— 质心几乎必然落在二维码内部。
		const content = 0.05
		best, bestD := -1, 1<<30
		for i := 0; i < n; i++ {
			if rate(i) < content {
				continue
			}
			d := i - hint
			if d < 0 {
				d = -d
			}
			if d < bestD {
				best, bestD = i, d
			}
		}
		if best < 0 {
			return hint
		}
		return best
	}
	// 暗像素质心（同时用于行列种子）。
	var sumX, sumY float64
	darkCount := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if dark[y*w+x] {
				sumX += float64(x)
				sumY += float64(y)
				darkCount++
			}
		}
	}
	if darkCount < 200 {
		return raw
	}
	hintY, hintX := int(sumY/float64(darkCount)), int(sumX/float64(darkCount))
	rowRate := func(y int) float64 {
		n := 0
		for x := 0; x < w; x++ {
			if dark[y*w+x] {
				n++
			}
		}
		return float64(n) / float64(w)
	}
	minGap := h / 60
	if minGap < 5 {
		minGap = 5
	}
	y0, y1 := extend(h, rowRate, peak(h, rowRate, hintY), minGap)
	if y1-y0 < 60 {
		return raw
	}
	colRate := func(x int) float64 {
		n := 0
		for y := y0; y < y1; y++ {
			if dark[y*w+x] {
				n++
			}
		}
		return float64(n) / float64(y1-y0)
	}
	x0, x1 := extend(w, colRate, peak(w, colRate, hintX), minGap)
	if x1-x0 < 60 {
		return raw
	}
	bw, bh := x1-x0, y1-y0
	// 二维码基本正方形；比例离谱说明投影抓到了别的竖条（侧边栏、分隔线），不裁。
	ratio := float64(bw) / float64(bh)
	if ratio < 0.72 || ratio > 1.4 {
		return raw
	}
	pad := int(float64(bw+bh) / 2 * 0.08)
	if pad < 4 {
		pad = 4
	}
	x0, y0 = x0-pad, y0-pad
	x1, y1 = x1+pad, y1+pad
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 > w {
		x1 = w
	}
	if y1 > h {
		y1 = h
	}
	// 只裁掉了一圈边才值得重新编码，否则保持原图（避免无谓的画质损失）。
	if (x1-x0)*(y1-y0) > w*h*85/100 {
		return raw
	}
	out := image.NewRGBA(image.Rect(0, 0, x1-x0, y1-y0))
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			out.Set(x-x0, y-y0, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	var buf bytes.Buffer
	if png.Encode(&buf, out) != nil {
		return raw
	}
	return buf.Bytes()
}

func cdpQRScreenshot(port int, platform string) (string, error) {
	ws, pageURL, err := cdpQRPickPage(port, platform)
	if err != nil {
		return "", err
	}
	defer ws.Close()

	browserQRPrepareOnce(port, platform, ws, pageURL)
	// 二维码由登录页 JS 异步渲染：点完入口 / 页面刚打开时元素还没画出来，
	// 而且页面里常并存同尺寸的装饰插画（豆包礼盒）和空白占位框。
	// 所以每一轮都要「取多个候选框 → 逐个截图 → 校验图像内容」，只有真二维码
	// 才返回；全部校验不通过就继续等待重试，绝不把装饰图或空白帧发给用户。
	const maxAttempts = 10
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// 营销弹窗可能晚于登录弹窗出现，所以每轮都清一次遮挡层；
		// 对没有这类弹窗的平台是无副作用空操作。
		_, _ = caEval(ws, browserQRDismissJS)
		boxes := cdpFindQRBoxesDOM(ws)
		if len(boxes) == 0 {
			// DOM 域拿不到时退回网页 JS（同源 iframe / shadow DOM 场景）。
			if raw, eerr := caEval(ws, browserQRFindJS); eerr == nil {
				var found *browserQRBox
				if json.Unmarshal(raw, &found) == nil && found != nil {
					boxes = append(boxes, *found)
				}
			}
		}
		// 小框优先：同一个登录页里，二维码本体的框永远比「包着它的卡片」小，
		// 先试小框能少截一层装饰，返回给用户的图也更紧凑。
		sort.SliceStable(boxes, func(i, j int) bool {
			return boxes[i].Width*boxes[i].Height < boxes[j].Width*boxes[j].Height
		})
		for _, box := range boxes {
			if box.Width < 100 || box.Height < 100 {
				continue
			}
			raw, serr := doubaoCDPCall(ws, 11, "Page.captureScreenshot", map[string]any{
				"format":                "png",
				"fromSurface":           true,
				"captureBeyondViewport": false,
				"clip": map[string]any{
					"x": box.X, "y": box.Y, "width": box.Width, "height": box.Height, "scale": 1,
				},
			}, 20*time.Second)
			if serr != nil {
				continue
			}
			var shot struct {
				Data string `json:"data"`
			}
			if json.Unmarshal(raw, &shot) != nil || shot.Data == "" {
				continue
			}
			if decoded, derr := base64.StdEncoding.DecodeString(shot.Data); derr == nil && qrShotLooksLikeQR(decoded) {
				// 校验通过后再裁边：先保证「是二维码」，再把卡片留白/标题裁掉，
				// 拿到的是「只有二维码」的紧凑图。
				return "data:image/png;base64," + base64.StdEncoding.EncodeToString(trimQRShot(decoded)), nil
			}
		}
		if attempt < maxAttempts-1 {
			time.Sleep(400 * time.Millisecond)
		}
	}
	return "", fmt.Errorf("二维码尚未渲染（%s），请稍候", pageURL)
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
