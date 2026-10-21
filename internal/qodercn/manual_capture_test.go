package qodercn

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omnigate/internal/auth"
)

// TestManualCaptureQueueFrame 手工探针（默认跳过）：把上游原始 SSE 落到
// data/qodercn-queue-capture.txt，用来确认真实排队帧长什么样。
// 运行：QODERCN_CAPTURE=1 go test ./internal/qodercn/ -run TestManualCaptureQueueFrame -v
func TestManualCaptureQueueFrame(t *testing.T) {
	if os.Getenv("QODERCN_CAPTURE") == "" {
		t.Skip("需要 QODERCN_CAPTURE=1 且本机有 qodercn 账号")
	}
	dir := os.Getenv("WILDWORK_HOME")
	if dir == "" {
		dir = `E:\陈焕生\omnigate`
	}
	auths, err := auth.LoadQoderCNDir(filepath.Join(dir, "auths"))
	if err != nil || len(auths) == 0 {
		t.Fatalf("加载账号失败: %v", err)
	}
	a := auths[0]
	c := New()
	c.HTTP.Timeout = 0
	body := []byte(`{"model":"deepseek-flash","messages":[{"role":"user","content":"say OK"}],"max_tokens":20,"stream":true}`)
	resp, status, respBody, err := c.ChatStream(a, body)
	if err != nil {
		t.Fatalf("ChatStream err=%v status=%d body=%s", err, status, string(respBody))
	}
	if resp == nil {
		t.Fatalf("no stream: status=%d body=%s", status, string(respBody))
	}
	defer resp.Close()
	out, _ := os.Create(`D:\Code\omnigate\data-qodercn-capture.txt`)
	defer out.Close()
	start := time.Now()
	br := bufio.NewReaderSize(resp, 256*1024)
	for {
		line, rerr := br.ReadString('\n')
		if line != "" {
			fmt.Fprintf(out, "[%6.1fs] %s", time.Since(start).Seconds(), line)
		}
		if rerr != nil {
			fmt.Fprintf(out, "[END] err=%v elapsed=%.1fs\n", rerr, time.Since(start).Seconds())
			break
		}
	}
	if b, rerr := io.ReadAll(out); rerr == nil {
		t.Logf("capture %d bytes", len(b))
	}
	_ = http.StatusOK
	_ = json.Marshal
	_ = strings.TrimSpace
}
