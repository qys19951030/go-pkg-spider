package collect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// testSite 描述一个本地测试站点: host 是页面里使用的域名(无显式端口),
// 真实流量经自定义 dialer 转发到 httptest 的本地 listener。
type testSite struct {
	host    string
	server  *httptest.Server
	mu      sync.Mutex
	ongoing int
	maxSeen int
}

// testEnv 封装一组本地站点与可注入 collect 的取页函数。
type testEnv struct {
	sites     map[string]*testSite
	transport *http.Transport
}

func newTestEnv(t *testing.T, siteSpecs map[string]http.HandlerFunc) *testEnv {
	t.Helper()

	env := &testEnv{sites: make(map[string]*testSite)}

	for host, handler := range siteSpecs {
		site := &testSite{host: host}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			site.mu.Lock()
			site.ongoing++
			if site.ongoing > site.maxSeen {
				site.maxSeen = site.ongoing
			}
			site.mu.Unlock()

			handler(w, r)

			site.mu.Lock()
			site.ongoing--
			site.mu.Unlock()
		}))
		t.Cleanup(srv.Close)
		site.server = srv
		env.sites[host] = site
	}

	// 自定义 dialer: 仅允许连接测试域名对应的本地 listener,
	// 任何其他地址一律拒绝, 保证用例不访问外网。
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	env.transport = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, splitErr := net.SplitHostPort(addr)
			if splitErr != nil {
				return nil, splitErr
			}
			site, ok := env.sites[host]
			if !ok {
				return nil, fmt.Errorf("external network access is disabled in tests: %s", addr)
			}
			_, localPort, err := net.SplitHostPort(strings.TrimPrefix(site.server.URL, "http://"))
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, "tcp", "127.0.0.1:"+localPort)
		},
	}

	return env
}

// fetch 注入给 Config.Fetch 的取页函数, 只走本地 transport。
func (e *testEnv) fetch(u string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("non-200 status")
	}
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if readErr != nil {
			break
		}
	}
	return buf, nil
}

func (e *testEnv) maxConcurrent(host string) int {
	site := e.sites[host]
	site.mu.Lock()
	defer site.mu.Unlock()
	return site.maxSeen
}

// htmlPage 组装一个带 title 的简单 HTML 页面。
func htmlPage(title string, body string) string {
	return fmt.Sprintf(`<!DOCTYPE html><html lang="zh"><head><meta charset="utf-8"><title>%s</title></head><body>%s</body></html>`, title, body)
}

// articlePage 生成带足够中文正文的内容页, 正文包裹在 article div 中,
// 各段落内容不同, 满足正文抽取器的节点得分要求。
func articlePage(title string, paras ...string) string {
	var b strings.Builder
	b.WriteString(`<div class="article">`)
	for _, p := range paras {
		b.WriteString("<p>" + p + "</p>")
	}
	b.WriteString(`</div>`)
	return htmlPage(title, b.String())
}

// articleParas 生成 n 段互不相同的中文长段落。
func articleParas(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf(
			"这是第%d段足够长的中文新闻正文内容用于语种探测和正文抽取, 句子长度各不相同保证方差不为零从而节点得分有效, 再补充一些文字让正文更厚实稳定可靠。",
			i+1)
	}
	return out
}

// listPage 生成列表页, entries 为 (href, 锚文本)。
func listPage(title string, entries [][2]string) string {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(fmt.Sprintf(`<a href="%s">%s</a>`, e[0], e[1]))
	}
	return htmlPage(title, b.String())
}
