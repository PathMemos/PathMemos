package sharecard

import "testing"

func TestValidateCaps(t *testing.T) {
	ok := func() *ShareCardRequest {
		return &ShareCardRequest{
			Header:     CardHeader{Title: "10月2日", Subtitle: "星期五"},
			CountLabel: "共 1 条记录",
			Records: []CardRecord{{
				TimeText: "08:30", MemberName: "妈妈", Text: "今天去了公园",
				Address: "朝阳公园", Images: []string{"https://example.com/a.jpg"},
			}},
			QR: CardQR{URL: "https://papafeiji.cn/qr"},
		}
	}
	if msg := validate(ok()); msg != "" {
		t.Fatalf("valid request rejected: %s", msg)
	}

	// 图片 URL 非法项被剔除而非拒绝
	req := ok()
	req.Records[0].Images = []string{"ftp://x", "javascript:1", "https://ok/a.jpg"}
	if msg := validate(req); msg != "" {
		t.Fatalf("mixed image urls rejected: %s", msg)
	}
	if len(req.Records[0].Images) != 1 {
		t.Fatalf("expected non-http urls dropped, got %v", req.Records[0].Images)
	}

	// 超 9 图截断
	req = ok()
	for i := 0; i < 12; i++ {
		req.Records[0].Images = append(req.Records[0].Images, "https://example.com/x.jpg")
	}
	if msg := validate(req); msg != "" {
		t.Fatalf("over-9-images rejected: %s", msg)
	}
	if len(req.Records[0].Images) != shareCardMaxImgRec {
		t.Fatalf("expected truncation to %d, got %d", shareCardMaxImgRec, len(req.Records[0].Images))
	}

	// 记录数超上限拒绝
	req = ok()
	for i := 0; i < shareCardMaxRecords+1; i++ {
		req.Records = append(req.Records, CardRecord{})
	}
	if msg := validate(req); msg != "too many records" {
		t.Fatalf("expected too many records, got %q", msg)
	}

	// QR 非法 URL 拒绝（QR 是卡片增长关键，硬校验）
	req = ok()
	req.QR.URL = "notaurl"
	if msg := validate(req); msg != "qr.url invalid" {
		t.Fatalf("expected qr.url invalid, got %q", msg)
	}

	// 封面非 http URL 静默降级为空
	req = ok()
	req.CoverImg = "file:///etc/passwd"
	if msg := validate(req); msg != "" || req.CoverImg != "" {
		t.Fatalf("expected cover downgraded to empty, got %q / %q", msg, req.CoverImg)
	}
}

func TestPayloadHashDeterministic(t *testing.T) {
	a := &ShareCardRequest{Header: CardHeader{Title: "T"}, Records: []CardRecord{{Text: "x"}}}
	b := &ShareCardRequest{Header: CardHeader{Title: "T"}, Records: []CardRecord{{Text: "x"}}}
	if payloadHash(a) != payloadHash(b) {
		t.Fatal("identical payloads must hash equal")
	}
	b.Records[0].Text = "y"
	if payloadHash(a) == payloadHash(b) {
		t.Fatal("different payloads must hash differ")
	}
}

func TestWrapCJK(t *testing.T) {
	loadFaces()
	if faceErr != nil {
		t.Skipf("font unavailable: %v", faceErr)
	}
	lines := wrap(faces.body, "一二三四五六七八九十", 148) // body 37px → 每行 4 字
	if len(lines) != 3 || lines[0] != "一二三四" {
		t.Fatalf("unexpected wrap: %q", lines)
	}
}
