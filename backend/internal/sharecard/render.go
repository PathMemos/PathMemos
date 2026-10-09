// Package sharecard 服务端渲染日记分享卡片（方案 A：分享图服务端出图）。
//
// 背景：客户端 canvas 出图依赖 Android 端 XWEB 扩展 SDK，且 App 端分享对缩略图
// 有 64KB 硬约束。服务端统一出图后：小程序/App 下载同一张图，XWEB 依赖可移除
// （基座瘦身），缩略图由服务端确定性压至 ≤60KB。
//
// 数据契约：客户端把已按 locale 格式化好的展示字符串（标题/副标题/计数/口号等）
// 整包 POST 过来，服务端是纯「文本→像素」引擎，不感知 i18n；记录文本/昵称/地址
// 原样透传。响应为 {poster, thumb} 两个可下载 URL（端点登记见
// docs/spec/03-api.md §8.6 POST /diary/share-card）。
//
// 绘制层不引入 fogleman/gg：填充矩形/圆角/直线/文字全部用 stdlib image +
// golang.org/x/image/font.Drawer 表达（go.mod 已有 x/image），减少依赖面。
//
// 版式：暖米色页面底 + 白色圆角卡片内嵌全部内容；封面(地图)圆角内嵌不顶边；
// 日期/地址/时间等粗体文字以多次偏移绘制模拟字重（x/image 无可变字重 API）；
// 二维码压平到白底缩小置于右下；emoji 会被整段去除（内嵌字体无 emoji 字形）。
package sharecard

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	// 海报素材可能为 PNG/GIF（存储原图不限格式），注册解码器。
	_ "image/gif"
	_ "image/png"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

//go:embed assets/NotoSansSC-Regular.otf
var fontRegularBytes []byte

//go:embed assets/NotoSansSC-Bold.otf
var fontBoldBytes []byte

// ---- 视觉参数（rpx×1.44 换算到 1080 宽画布）----

const (
	cardW      = 1080.0
	frame      = 46.0  // 页面留白（暖米色底露出部分）
	cardPad    = 35.0  // 卡片内边距
	cardR      = 40.0  // 卡片圆角
	coverH     = 750.0 // 封面(地图)高（520rpx）
	coverR     = 40.0  // 封面圆角
	titleH     = 112.0 // 日期 + 周几同行块高
	timeCol    = 115.0 // 记录时间列宽
	timeGap    = 35.0  // 时间列与主列间距
	lineH      = 66.0  // 记录内容行高（46rpx）
	recTop     = 24.0  // 记录块顶部留白
	addrLineH  = 56.0  // 地址行高（28rpx 加大加粗，超长自动折行）
	addrGap    = 24.0  // 地址与内容间距
	recGap     = 52.0  // 记录块间距（36rpx）
	imgGridH   = 253.0 // 多图网格高（176rpx）
	imgOneH    = 518.0 // 单图高（360rpx）
	imgGap     = 17.0  // 图间距（12rpx）
	footerQR   = 190.0 // 二维码展示尺寸（132rpx，白底不抢主内容）
	footerPadB = 12.0  // 尾部二维码行下方留白
	canvasCapH = 10000.0
	fetchCap   = 12 << 20
	fetchWait  = 10 * time.Second
	jpegQ      = 90

	thumbW = 240 // 缩略图宽
	thumbQ = 80
)

// 渲染并发闸门：单次渲染峰值内存约 45MB（1080×10000 RGBA），限并发防叠加。
var renderGate = make(chan struct{}, 3)

var palette = struct{ bg, cardBg, textMain, textSub, textContent, textLight, accent, accentLight, border string }{
	bg:          "#F5F2ED",
	cardBg:      "#FFFFFF",
	textMain:    "#2C2419",
	textSub:     "#7A7169",
	textContent: "#8A8179",
	textLight:   "#A8A099",
	accent:      "#8B6F4E",
	accentLight: "#F5F0E8",
	border:      "#E5E0D8",
}

// ---- 请求契约（字段均为客户端按 locale 格式化好的展示串）----

type CardHeader struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle"`
}

type CardRecord struct {
	TimeText   string   `json:"timeText"`
	MemberName string   `json:"memberName"`
	Text       string   `json:"text"`
	Address    string   `json:"address"`
	Images     []string `json:"images"`
}

type CardQR struct {
	URL   string `json:"url"`
	Label string `json:"label"`
}

// ShareCardRequest POST /diary/share-card 请求体。
type ShareCardRequest struct {
	Header     CardHeader   `json:"header"`
	Brand      string       `json:"brand"` // 已不展示（封面右上品牌已移除），保留字段兼容客户端
	MemberPill []string     `json:"memberPills"`
	CoverImg   string       `json:"coverImg"`
	CountLabel string       `json:"countLabel"`
	Records    []CardRecord `json:"records"`
	EmptyTitle string       `json:"emptyTitle"`
	EmptyTip   string       `json:"emptyTip"`
	Slogan     string       `json:"slogan"`
	SubSlogan  string       `json:"subSlogan"`
	QR         CardQR       `json:"qr"`
}

// ---- 字体 ----

type faceSet struct {
	title, dateSub, count, time, addr, member, body, slogan, subSlogan, emptyTip font.Face
}

var (
	faceOnce sync.Once
	faces    faceSet
	faceErr  error
)

func loadFaces() {
	reg, err := opentype.Parse(fontRegularBytes)
	if err != nil {
		faceErr = fmt.Errorf("parse font regular: %w", err)
		return
	}
	bold, err := opentype.Parse(fontBoldBytes)
	if err != nil {
		faceErr = fmt.Errorf("parse font bold: %w", err)
		return
	}
	mk := func(f *opentype.Font, px float64) font.Face {
		face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: px, DPI: 72, Hinting: font.HintingFull})
		if err != nil {
			faceErr = err
			return nil
		}
		return face
	}
	// 标题/时间/地址/口号用真 Bold 字重——正文/标题用静态 Bold/Regular 双字重。
	faces = faceSet{
		title:     mk(bold, 62), // 日期大标题（44rpx）
		dateSub:   mk(reg, 37),  // 周几（26rpx）
		count:     mk(reg, 37),  // 计数行（26rpx）
		time:      mk(bold, 40), // 记录时间（28rpx）
		addr:      mk(bold, 40), // 地址标题（28rpx）
		member:    mk(reg, 30),  // 记录归属成员名（20rpx）
		body:      mk(reg, 37),  // 记录内容（26rpx）
		slogan:    mk(bold, 40), // 口号（28rpx）
		subSlogan: mk(reg, 32),  // 副口号（22rpx）
		emptyTip:  mk(reg, 37),  // 空态提示
	}
}

// ---- 基础工具 ----

func wrap(face font.Face, text string, maxW float64) []string {
	d := &font.Drawer{Face: face}
	width := func(s string) float64 { return float64(d.MeasureString(s) >> 6) }
	lines := []string{}
	for _, para := range strings.Split(text, "\n") {
		if para == "" {
			lines = append(lines, "")
			continue
		}
		line := ""
		for _, r := range para {
			trial := line + string(r)
			if width(trial) > maxW && line != "" {
				lines = append(lines, line)
				line = string(r)
				continue
			}
			line = trial
		}
		lines = append(lines, line)
	}
	return lines
}

// （02b §5 share-card 行）：重定向每跳复用与首跳同语义的黑名单校验——
// Go 默认跨 scheme 跟随（https→http 也跟），无此钩子时「公网 https URL → 302 →
// 内网 http 地址」恰好从 isBlockedImageHost 的正门绕出。上限 3 跳防循环；
// 校验失败按既有抓取失败语义处理（跳过/占位），不改变错误路径。
var httpClient = &http.Client{
	Timeout: fetchWait,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("too many redirects")
		}
		if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
			return fmt.Errorf("redirect scheme not http(s)")
		}
		if isBlockedImageHost(req.URL.Hostname()) {
			return fmt.Errorf("redirect host not allowed")
		}
		return nil
	},
}

// isBlockedImageHost 私网/回环/元数据地址黑名单：分享卡图片 URL 来自客户端输入
// （coverImg/qr.url），盲 SSRF（仅图片解码无回显）也不应允许源站向内网发起请求。
// 解析后逐 IP 判断（域名可能解析到私网），解析失败一律拒绝。
func isBlockedImageHost(host string) bool {
	host = strings.ToLower(host)
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return true
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return true
		}
		// CGNAT 100.64.0.0/10（含阿里云元数据 100.100.100.200）：IsPrivate 不覆盖该段
		if b := ip.To4(); b != nil && b[0] == 100 && b[1] >= 64 && b[1] <= 127 {
			return true
		}
	}
	return false
}

func fetchImage(ctx context.Context, rawURL string) (image.Image, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("image url not http(s)")
	}
	if isBlockedImageHost(u.Hostname()) {
		return nil, fmt.Errorf("image host not allowed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("image fetch status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, fetchCap))
	if err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	return img, err
}

// fetchAll 并发抓取（信号量上限 6），失败项返回 nil 占位。
func fetchAll(ctx context.Context, urls []string) []image.Image {
	out := make([]image.Image, len(urls))
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i, u := range urls {
		if u == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, u string) {
			defer wg.Done()
			defer func() { <-sem }()
			if img, err := fetchImage(ctx, u); err == nil {
				out[i] = img
			}
		}(i, u)
	}
	wg.Wait()
	return out
}

func scaleTo(src image.Image, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	return dst
}

// aspectFill 等比放大居中裁切到 w×h。
func aspectFill(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw < 1 || sh < 1 {
		return image.NewRGBA(image.Rect(0, 0, w, h))
	}
	scale := math.Max(float64(w)/float64(sw), float64(h)/float64(sh))
	cw := int(float64(w) / scale)
	ch := int(float64(h) / scale)
	if cw > sw {
		cw = sw
	}
	if ch > sh {
		ch = sh
	}
	cx := b.Min.X + (sw-cw)/2
	cy := b.Min.Y + (sh-ch)/2
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, image.Rect(cx, cy, cx+cw, cy+ch), draw.Src, nil)
	return dst
}

// roundRGBA 圆角裁切：圆角外像素置透明。
func roundRGBA(img *image.RGBA, r float64) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			rx, ry := float64(x)+0.5, float64(y)+0.5
			var dx, dy float64
			if rx < r {
				dx = r - rx
			} else if rx > float64(w)-r {
				dx = rx - (float64(w) - r)
			}
			if ry < r {
				dy = r - ry
			} else if ry > float64(h)-r {
				dy = ry - (float64(h) - r)
			}
			if dx*dx+dy*dy > r*r {
				img.SetRGBA(x, y, color.RGBA{})
			}
		}
	}
}

// flattenOnCard 把带透明通道的图压平到卡片白底——JPEG 无 alpha，
// 圆角裁切/透明码图直接编码会整块变黑。
func flattenOnCard(img image.Image) *image.RGBA {
	b := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(hexCol(palette.cardBg)), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Over)
	return dst
}

// stripEmoji 去掉 emoji 及其连接/修饰符：内嵌的 NotoSansSC 无 emoji 字形，
// 直接渲染会得到空心豆腐块或空白；去除后压缩多余空格保持版面整洁。
func stripEmoji(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 0x1F000 && r <= 0x1FFFD, // emoji 主区（含表情/物品/旗帜）
			r >= 0x2600 && r <= 0x27BF, // 杂项符号与装饰（☀☁✂✈…）
			r >= 0x2B00 && r <= 0x2BFF, // 箭头/星形（⬆⭐…）
			r >= 0x2190 && r <= 0x21FF, // 箭头符号
			r >= 0xFE00 && r <= 0xFE0F, // 变体选择符
			r == 0x200D,                // 零宽连接符
			r >= 0xE000 && r <= 0xF8FF: // 私用区
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	for strings.Contains(out, "  ") {
		out = strings.ReplaceAll(out, "  ", " ")
	}
	return strings.TrimSpace(out)
}

// ---- 画布原语（gg 替代层）----

type canvas struct {
	img *image.RGBA
}

func newCanvas(w, h int, bg color.Color) *canvas {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)
	return &canvas{img: img}
}

func hexCol(hex string) color.Color {
	var r, g, b uint8
	fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b) //nolint:errcheck
	return color.RGBA{R: r, G: g, B: b, A: 0xff}
}

func (c *canvas) fillRect(x, y, w, h float64, col color.Color) {
	draw.Draw(c.img, image.Rect(int(x), int(y), int(x+w), int(y+h)), image.NewUniform(col), image.Point{}, draw.Src)
}

func (c *canvas) fillCircle(cx, cy, r float64, col color.Color) {
	u := image.NewUniform(col)
	for py := int(cy - r); py < int(cy+r)+1; py++ {
		for px := int(cx - r); px < int(cx+r)+1; px++ {
			dx, dy := float64(px)+0.5-cx, float64(py)+0.5-cy
			if dx*dx+dy*dy <= r*r {
				c.img.Set(px, py, u)
			}
		}
	}
}

// fillRoundRect 圆角矩形：两块矩形拼十字 + 四角整圆（同色叠加无锯齿缝）。
func (c *canvas) fillRoundRect(x, y, w, h, r float64, col color.Color) {
	if r > w/2 {
		r = w / 2
	}
	if r > h/2 {
		r = h / 2
	}
	c.fillRect(x, y+r, w, h-2*r, col)
	c.fillRect(x+r, y, w-2*r, h, col)
	c.fillCircle(x+r, y+r, r, col)
	c.fillCircle(x+w-r, y+r, r, col)
	c.fillCircle(x+r, y+h-r, r, col)
	c.fillCircle(x+w-r, y+h-r, r, col)
}

func (c *canvas) drawImage(img image.Image, x, y int) {
	draw.Draw(c.img, image.Rect(x, y, x+img.Bounds().Dx(), y+img.Bounds().Dy()), img, image.Point{}, draw.Src)
}

func textWidth(face font.Face, s string) float64 {
	d := &font.Drawer{Face: face}
	return float64(d.MeasureString(s) >> 6)
}

// text 顶部对齐画文字。
func (c *canvas) text(face font.Face, colHex, s string, x, yTop float64) {
	if s == "" {
		return
	}
	baseline := yTop + float64(face.Metrics().Ascent)/64
	d := &font.Drawer{
		Dst:  c.img,
		Src:  image.NewUniform(hexCol(colHex)),
		Face: face,
		Dot:  fixed.P(int(x), int(baseline)),
	}
	d.DrawString(s)
}

func (c *canvas) divider(x1, x2, y float64) {
	c.fillRect(x1, y-1, x2-x1, 2, hexCol(palette.border))
}

// ---- 渲染 ----

type sizedRecord struct {
	rec       *CardRecord
	imgs      []*image.RGBA
	lines     []string
	addrLines []string
	height    float64
}

// Render 渲染海报（JPEG）与缩略图（JPEG ≤60KB 口径）。
func Render(ctx context.Context, req *ShareCardRequest) ([]byte, []byte, error) {
	faceOnce.Do(loadFaces)
	if faceErr != nil {
		return nil, nil, faceErr
	}
	renderGate <- struct{}{}
	defer func() { <-renderGate }()

	// 内嵌字体无 emoji 字形：统一去除，避免豆腐块/空白
	req.Header.Title = stripEmoji(req.Header.Title)
	req.Header.Subtitle = stripEmoji(req.Header.Subtitle)
	req.CountLabel = stripEmoji(req.CountLabel)
	req.Slogan = stripEmoji(req.Slogan)
	req.SubSlogan = stripEmoji(req.SubSlogan)
	req.EmptyTitle = stripEmoji(req.EmptyTitle)
	req.EmptyTip = stripEmoji(req.EmptyTip)
	for i := range req.Records {
		req.Records[i].TimeText = stripEmoji(req.Records[i].TimeText)
		req.Records[i].MemberName = stripEmoji(req.Records[i].MemberName)
		req.Records[i].Text = stripEmoji(req.Records[i].Text)
		req.Records[i].Address = stripEmoji(req.Records[i].Address)
	}

	contentX := frame + cardPad
	innerW := cardW - frame*2 - cardPad*2
	mainX := contentX + timeCol + timeGap
	mainW := innerW - timeCol - timeGap

	// ---- 素材抓取（并发，失败跳过；QR 失败硬错——无码卡片无增长价值）----
	var qrImg, coverImg *image.RGBA
	if req.QR.URL != "" {
		img, err := fetchImage(ctx, req.QR.URL)
		if err != nil {
			return nil, nil, fmt.Errorf("fetch qr: %w", err)
		}
		// 先压平透明通道到卡片白底（JPEG 直编透明→黑块），再缩到展示尺寸
		qrImg = scaleTo(flattenOnCard(img), int(footerQR), int(footerQR))
	}
	if req.CoverImg != "" {
		if img, err := fetchImage(ctx, req.CoverImg); err == nil {
			cover := aspectFill(img, int(innerW), int(coverH))
			roundRGBA(cover, coverR)
			coverImg = flattenOnCard(cover) // 圆角外透明区压平到白底，防 JPEG 黑角
		}
	}
	recs := make([]sizedRecord, 0, len(req.Records))
	for i := range req.Records {
		r := &req.Records[i]
		n := len(r.Images)
		imgW, imgH := mainW, imgOneH
		if n > 1 {
			imgW, imgH = (mainW-2*imgGap+36)/3, imgGridH
		}
		rgba := make([]*image.RGBA, 0, n)
		for _, im := range fetchAll(ctx, r.Images) {
			if im != nil {
				scaled := aspectFill(im, int(imgW), int(imgH))
				roundRGBA(scaled, 22)
				rgba = append(rgba, flattenOnCard(scaled))
			}
		}
		recs = append(recs, sizedRecord{rec: r, imgs: rgba})
	}

	// ---- 布局测高（与下方绘制严格同序同值）----
	// 分享卡固定展示全部记录。
	H := frame*2 + cardPad*2
	H += coverH
	H += 50 + titleH + 36
	if req.CountLabel != "" {
		H += 64
	}
	if len(recs) == 0 && req.EmptyTitle != "" {
		H += 240
	}
	for i := range recs {
		s := &recs[i]
		s.lines = wrap(faces.body, s.rec.Text, mainW)
		// 地址超长自动折行（宽度扣除定位圆点区 40px）
		if s.rec.Address != "" {
			s.addrLines = wrap(faces.addr, s.rec.Address, mainW-40)
		}
		h := recTop
		if len(s.addrLines) > 0 {
			h += float64(len(s.addrLines))*addrLineH + addrGap
		}
		h += float64(len(s.lines)) * lineH
		if n := len(s.imgs); n > 0 {
			if n == 1 {
				h += imgOneH + 22
			} else {
				rows := (n + 2) / 3
				h += float64(rows)*imgGridH + float64(rows-1)*imgGap + 22
			}
		}
		h += recGap
		s.height = h
		H += h
	}
	H += 2 + 35 + footerQR + footerPadB
	if H > canvasCapH {
		H = canvasCapH
	}

	// ---- 绘制 ----
	c := newCanvas(int(cardW), int(H), hexCol(palette.bg))
	// 卡片柔和落影：6% 深棕预混入暖米底（JPEG 无 alpha，不能画半透明）
	c.fillRoundRect(frame+4, frame+7, cardW-frame*2, float64(H)-frame*2, cardR, hexCol("#E9E5DE"))
	// 白色圆角卡片浮在暖米色底上
	c.fillRoundRect(frame, frame, cardW-frame*2, float64(H)-frame*2, cardR, hexCol(palette.cardBg))

	y := frame + cardPad

	// 封面(地图)：圆角内嵌不顶边（已压平白底，无黑角）；品牌不再展示
	if coverImg != nil {
		c.drawImage(coverImg, int(contentX), int(y))
	}
	y += coverH + 50

	// 日期（大字加粗），周几同行右侧灰字
	c.text(faces.title, palette.textMain, req.Header.Title, contentX, y)
	if req.Header.Subtitle != "" {
		dw := textWidth(faces.title, req.Header.Title)
		c.text(faces.dateSub, palette.textSub, req.Header.Subtitle, contentX+dw+28, y+18)
	}
	y += titleH

	// 计数行
	if req.CountLabel != "" {
		c.text(faces.count, palette.textSub, req.CountLabel, contentX, y)
		y += 64
	}

	// 空态
	if len(recs) == 0 && req.EmptyTitle != "" {
		c.text(faces.slogan, palette.textMain, req.EmptyTitle, contentX, y+60)
		c.text(faces.emptyTip, palette.textLight, req.EmptyTip, contentX, y+150)
		y += 240
	}

	// 记录列表：时间列（左，主题色加粗）+ 主列（定位点/地址标题/内容/图片格），行间分隔线
	for i := range recs {
		s := &recs[i]
		top := y + recTop
		c.text(faces.time, palette.accent, s.rec.TimeText, contentX, top)

		mainY := top
		if len(s.addrLines) > 0 {
			// 定位点图标：主题色圆环 + 白芯，圆心对齐首行字面视觉中心
			//（= 文字顶 + (上行+下行)/2，用真实字体度量计算，不靠估算）
			m := faces.addr.Metrics()
			pinCY := top + (float64(m.Ascent)+float64(m.Descent))/2/64
			c.fillCircle(mainX+11, pinCY, 11, hexCol(palette.accent))
			c.fillCircle(mainX+11, pinCY, 4.5, hexCol(palette.cardBg))
			for li, line := range s.addrLines {
				c.text(faces.addr, palette.textMain, line, mainX+32, top+float64(li)*addrLineH)
			}
			// 成员名挂在末行行尾（末行通常较短，避免首行过长时被卡片右缘裁切）
			lastIdx := len(s.addrLines) - 1
			lastW := textWidth(faces.addr, s.addrLines[lastIdx])
			if s.rec.MemberName != "" {
				c.text(faces.member, palette.textLight, s.rec.MemberName, mainX+32+lastW+24, top+float64(lastIdx)*addrLineH+8)
			}
			mainY = top + float64(len(s.addrLines))*addrLineH + addrGap
		}
		for _, line := range s.lines {
			c.text(faces.body, palette.textContent, line, mainX, mainY)
			mainY += lineH
		}
		if len(s.imgs) > 0 {
			mainY += 22
			cols := len(s.imgs)
			if cols > 3 {
				cols = 3
			}
			iw := mainW
			ih := imgOneH
			if len(s.imgs) > 1 {
				iw = (mainW - float64(cols-1)*imgGap) / float64(cols)
				ih = imgGridH
			}
			for idx, im := range s.imgs {
				row, col := idx/cols, idx%cols
				c.drawImage(im, int(mainX+float64(col)*(iw+imgGap)), int(mainY+float64(row)*(ih+imgGap)))
			}
		}

		y += s.height
		if i < len(recs)-1 {
			c.divider(mainX, contentX+innerW, y-recGap/2)
		}
	}

	// 尾部：分隔线 + 口号（左两行）+ 二维码（右，白底小尺寸）
	c.divider(contentX, contentX+innerW, y)
	y += 35

	if qrImg != nil {
		qrX := contentX + innerW - footerQR
		c.drawImage(qrImg, int(qrX), int(y))
	}
	c.text(faces.slogan, palette.textMain, req.Slogan, contentX, y+30)
	c.text(faces.subSlogan, palette.textSub, req.SubSlogan, contentX, y+108)
	// 尾部下方留白由 H 的 footerPadB 控制（已收紧）

	// ---- 编码 ----
	poster, err := jpegEncode(c.img, jpegQ)
	if err != nil {
		return nil, nil, err
	}
	thumb, err := jpegEncode(scaleTo(c.img, thumbW, int(H)*thumbW/int(cardW)), thumbQ)
	if err != nil {
		return nil, nil, err
	}
	return poster, thumb, nil
}

func jpegEncode(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
