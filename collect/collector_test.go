package collect

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	hostA = "news.example.cn"
	// 不同顶级域, 用于验证只跟进同域链接
	hostB = "outside.example.org"
)

func loadRecords(t *testing.T, dir string) []Record {
	t.Helper()
	var out []Record
	for _, line := range readLines(t, filepath.Join(dir, recordsFile)) {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("unmarshal record %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

func loadFailures(t *testing.T, dir string) []Failure {
	t.Helper()
	var out []Failure
	for _, line := range readLines(t, filepath.Join(dir, failuresFile)) {
		var f Failure
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("unmarshal failure %q: %v", line, err)
		}
		out = append(out, f)
	}
	return out
}

// twoLevelEnv 构造两层站点: / 与 /list 为列表页, /news/n.html 为内容页,
// 并含一个站外链接(不应跟进)。
func twoLevelEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnv(t, map[string]http.HandlerFunc{
		hostA: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			switch r.URL.Path {
			case "/", "/index.html":
				_, _ = w.Write([]byte(listPage("示例新闻站点首页", [][2]string{
					{"/list", "新闻频道"},
					{"/news/1.html", "第一篇新闻文章的完整中文标题内容"},
				})))
			case "/list":
				_, _ = w.Write([]byte(listPage("新闻频道列表页面", [][2]string{
					{"/news/1.html", "第一篇新闻文章的完整中文标题内容"},
					{"/news/2.html", "第二篇新闻文章的完整中文标题内容"},
					{"/news/3.html", "第三篇新闻文章的完整中文标题内容"},
					{"https://" + hostB + "/external.html", "站外其他网站的一篇中文新闻文章标题"},
				})))
			case "/news/1.html", "/news/2.html", "/news/3.html":
				id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/news/"), ".html")
				paras := append(articleParas(10), "发布时间: 2026-03-0"+id+" 09:30:00")
				_, _ = w.Write([]byte(articlePage(
					"第"+id+"篇新闻文章的完整中文标题",
					paras...,
				)))
			default:
				http.NotFound(w, r)
			}
		},
		hostB: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(articlePage("站外文章完整中文标题内容", articleParas(10)...)))
		},
	})
}

func baseConfig(dir string, env *testEnv, mode Mode) Config {
	return Config{
		Seeds:       []string{"http://" + hostA + "/"},
		Depth:       2,
		OutputDir:   dir,
		Mode:        mode,
		Concurrency: 2,
		Fetch:       env.fetch,
	}
}

// TestRunEndToEnd 端到端: 链接分类、同域过滤、内容抽取、字段、计数、确定性。
func TestRunEndToEnd(t *testing.T) {
	env := twoLevelEnv(t)
	dir := t.TempDir()

	stats, err := Collect(baseConfig(dir, env, ModeFull))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	records := loadRecords(t, dir)
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3", len(records))
	}

	// 按 URL 升序、seq 从 1 连续
	urls := make([]string, len(records))
	for i, r := range records {
		urls[i] = r.Url
		if r.Seq != int64(i+1) {
			t.Fatalf("seq at %d = %d, want %d", i, r.Seq, i+1)
		}
		if r.Lang != "zh" {
			t.Fatalf("lang = %q, want zh", r.Lang)
		}
		if r.Content == "" || r.Title == "" {
			t.Fatalf("empty title/content: %+v", r)
		}
		if r.ListUrl == "" || r.Depth == 0 {
			t.Fatalf("missing list_url/depth: %+v", r)
		}
	}
	if !sort.StringsAreSorted(urls) {
		t.Fatalf("records not sorted by url: %v", urls)
	}
	// 规范化生效: URL 无 www、无锚点、无跟踪参数
	for _, u := range urls {
		if strings.Contains(u, "www.") || strings.Contains(u, "#") {
			t.Fatalf("url not normalized: %s", u)
		}
	}

	// 站外链接未跟进: hostB 不应出现在记录里
	for _, r := range records {
		if strings.Contains(r.Url, hostB) {
			t.Fatalf("cross-domain link followed: %s", r.Url)
		}
	}

	// 计数恒等式
	if stats.Planned != stats.Success+stats.Skipped+stats.Failed {
		t.Fatalf("stats identity broken: %+v", stats)
	}
	if stats.Success != 3 || stats.Failed != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	// failures 文件不应有内容
	if fs := loadFailures(t, dir); len(fs) != 0 {
		t.Fatalf("unexpected failures: %+v", fs)
	}

	// 同样的内容在另一个目录再跑一遍, records.jsonl 逐字节一致
	dir2 := t.TempDir()
	if _, err := Collect(baseConfig(dir2, env, ModeFull)); err != nil {
		t.Fatalf("Collect second: %v", err)
	}
	a, _ := os.ReadFile(filepath.Join(dir, recordsFile))
	b, _ := os.ReadFile(filepath.Join(dir2, recordsFile))
	if string(a) != string(b) {
		t.Fatalf("records.jsonl not byte-identical across runs")
	}
}

// TestRunDedupAndFullRerun 全量重跑不重复入库, 已发现重复链接保留首次溯源。
func TestRunDedupAndFullRerun(t *testing.T) {
	env := twoLevelEnv(t)
	dir := t.TempDir()

	cfg := baseConfig(dir, env, ModeFull)
	first, err := Collect(cfg)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if first.Success != 3 {
		t.Fatalf("first success = %d, want 3", first.Success)
	}

	// 全量模式重跑: 重新请求, 但全部跳过、不重写
	second, err := Collect(cfg)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.Success != 0 {
		t.Fatalf("second success = %d, want 0", second.Success)
	}
	if second.Skipped == 0 {
		t.Fatalf("second run should skip existing records: %+v", second)
	}
	if second.Planned != second.Success+second.Skipped+second.Failed {
		t.Fatalf("identity broken: %+v", second)
	}

	records := loadRecords(t, dir)
	if len(records) != 3 {
		t.Fatalf("records after rerun = %d, want 3 (no duplicate)", len(records))
	}
	// seq 仍是 1..3, 没有被追加
	for i, r := range records {
		if r.Seq != int64(i+1) {
			t.Fatalf("seq changed after rerun: %+v", r)
		}
	}

	// /news/1 同时在首页(depth 1)和 /list(depth 2)出现,
	// 必须保留第一次发现的 list_url(首页) 与 depth(1)
	var firstRecord *Record
	for i := range records {
		if strings.HasSuffix(records[i].Url, "/news/1.html") {
			firstRecord = &records[i]
		}
	}
	if firstRecord == nil {
		t.Fatal("news/1 record missing")
	}
	if firstRecord.Depth != 1 {
		t.Fatalf("news/1 depth = %d, want 1 (first discovery)", firstRecord.Depth)
	}
	if firstRecord.ListUrl != "http://"+hostA+"/" {
		t.Fatalf("news/1 list_url = %q, want seed root", firstRecord.ListUrl)
	}
}

// TestRunIncrementalSkip 增量模式: checkpoint 已完成的 URL 不再请求。
func TestRunIncrementalSkip(t *testing.T) {
	var contentHits int32
	env := newTestEnv(t, map[string]http.HandlerFunc{
		hostA: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			switch r.URL.Path {
			case "/":
				_, _ = w.Write([]byte(listPage("首页列表", [][2]string{
					{"/news/1.html", "第一篇新闻文章的完整中文标题内容"},
					{"/news/2.html", "第二篇新闻文章的完整中文标题内容"},
				})))
			case "/news/1.html", "/news/2.html":
				atomic.AddInt32(&contentHits, 1)
				id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/news/"), ".html")
				_, _ = w.Write([]byte(articlePage("第"+id+"篇新闻文章标题内容", articleParas(10)...)))
			default:
				http.NotFound(w, r)
			}
		},
	})

	dir := t.TempDir()
	cfg := Config{
		Seeds:       []string{"http://" + hostA + "/"},
		Depth:       1,
		OutputDir:   dir,
		Mode:        ModeFull,
		Concurrency: 2,
		Fetch:       env.fetch,
	}

	first, err := Collect(cfg)
	if err != nil {
		t.Fatalf("full run: %v", err)
	}
	if first.Success != 2 {
		t.Fatalf("first success = %d, want 2", first.Success)
	}
	if atomic.LoadInt32(&contentHits) != 2 {
		t.Fatalf("content hits = %d, want 2", contentHits)
	}

	// 增量重跑: 已完成内容页不再请求
	atomic.StoreInt32(&contentHits, 0)
	cfg.Mode = ModeIncremental
	second, err := Collect(cfg)
	if err != nil {
		t.Fatalf("incremental run: %v", err)
	}
	if atomic.LoadInt32(&contentHits) != 0 {
		t.Fatalf("incremental requested completed urls, hits = %d", contentHits)
	}
	if second.Success != 0 || second.Skipped < 2 {
		t.Fatalf("incremental stats wrong: %+v", second)
	}
	if second.Planned != second.Success+second.Skipped+second.Failed {
		t.Fatalf("identity broken: %+v", second)
	}

	// 记录数不变, checkpoint 与记录对齐
	records := loadRecords(t, dir)
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	cpData, err := os.ReadFile(filepath.Join(dir, checkpointFile))
	if err != nil {
		t.Fatal(err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(cpData, &cp); err != nil {
		t.Fatal(err)
	}
	if cp.Count != 2 || len(cp.Completed) != 2 {
		t.Fatalf("checkpoint mismatch: %+v", cp)
	}
}

// TestRunResumeAfterCrash 模拟进程在两批之间被杀: 第一批全量采一部分,
// 之后用同一输出目录增量重启, 已有数据保留、同一篇不重复入库。
func TestRunResumeAfterCrash(t *testing.T) {
	env := newTestEnv(t, map[string]http.HandlerFunc{
		hostA: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			switch r.URL.Path {
			case "/":
				_, _ = w.Write([]byte(listPage("首页列表", [][2]string{
					{"/news/1.html", "第一篇新闻文章的完整中文标题内容"},
					{"/news/2.html", "第二篇新闻文章的完整中文标题内容"},
					{"/news/3.html", "第三篇新闻文章的完整中文标题内容"},
				})))
			case "/news/1.html", "/news/2.html", "/news/3.html":
				id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/news/"), ".html")
				_, _ = w.Write([]byte(articlePage("第"+id+"篇新闻文章的完整中文标题", articleParas(10)...)))
			default:
				http.NotFound(w, r)
			}
		},
	})

	dir := t.TempDir()

	// 第一批: 种子只暴露前两篇(临时改页面不现实, 这里直接全量采 3 篇)
	first, err := Collect(Config{
		Seeds:       []string{"http://" + hostA + "/"},
		Depth:       1,
		OutputDir:   dir,
		Mode:        ModeFull,
		Concurrency: 1,
		Fetch:       env.fetch,
	})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if first.Success != 3 {
		t.Fatalf("first success = %d, want 3", first.Success)
	}

	// 模拟进程被杀后重启: 重新打开同一目录, 全量重跑也不允许重复追加
	second, err := Collect(Config{
		Seeds:       []string{"http://" + hostA + "/"},
		Depth:       1,
		OutputDir:   dir,
		Mode:        ModeFull,
		Concurrency: 1,
		Fetch:       env.fetch,
	})
	if err != nil {
		t.Fatalf("rerun after restart: %v", err)
	}
	if second.Success != 0 || second.Skipped != 3 {
		t.Fatalf("rerun stats wrong: %+v", second)
	}

	records := loadRecords(t, dir)
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3 (no duplicate after restart)", len(records))
	}
	// seq 仍然连续 1..3
	for i, r := range records {
		if r.Seq != int64(i+1) {
			t.Fatalf("seq not contiguous after restart: %+v", r)
		}
	}
}

// TestRunNewRecordAppendedSorted 增量运行发现新文章时, 追加块内部仍按 URL 升序,
// seq 在历史条数之后连续递增。
func TestRunNewRecordAppendedSorted(t *testing.T) {
	var knownPages = map[string]bool{
		"/news/1.html": true,
		"/news/2.html": true,
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/" {
			entries := [][2]string{
				{"/news/1.html", "第一篇新闻文章的完整中文标题内容"},
				{"/news/2.html", "第二篇新闻文章的完整中文标题内容"},
			}
			// 第二批开始后新增第三篇
			if knownPages["/news/3.html"] {
				entries = append(entries, [2]string{"/news/3.html", "第三篇新闻文章的完整中文标题内容"})
			}
			_, _ = w.Write([]byte(listPage("首页列表", entries)))
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/news/"), ".html")
		_, _ = w.Write([]byte(articlePage("第"+id+"篇新闻文章的完整中文标题", articleParas(10)...)))
	}
	env := newTestEnv(t, map[string]http.HandlerFunc{hostA: handler})
	dir := t.TempDir()
	cfg := Config{
		Seeds:       []string{"http://" + hostA + "/"},
		Depth:       1,
		OutputDir:   dir,
		Mode:        ModeIncremental,
		Concurrency: 1,
		Fetch:       env.fetch,
	}

	first, err := Collect(cfg)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Success != 2 {
		t.Fatalf("first success = %d, want 2", first.Success)
	}

	// 新文章上线
	knownPages["/news/3.html"] = true
	second, err := Collect(cfg)
	if err != nil {
		t.Fatalf("incremental second: %v", err)
	}
	if second.Success != 1 || second.Skipped != 2 {
		t.Fatalf("second stats wrong: %+v", second)
	}

	records := loadRecords(t, dir)
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3", len(records))
	}
	last := records[len(records)-1]
	if last.Url != "http://"+hostA+"/news/3.html" || last.Seq != 3 {
		t.Fatalf("appended block wrong: %+v", last)
	}
}

// TestRunFailureIsolation 单页失败不拖垮整批, 其他分支与种子照常, 数据保留。
func TestRunFailureIsolation(t *testing.T) {
	env := newTestEnv(t, map[string]http.HandlerFunc{
		hostA: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			switch r.URL.Path {
			case "/":
				_, _ = w.Write([]byte(listPage("首页列表", [][2]string{
					{"/dead-list", "失效栏目"},
					{"/news/1.html", "第一篇新闻文章的完整中文标题内容"},
					{"/news/bad.html", "一篇抓取失败的新闻文章完整标题"},
				})))
			case "/dead-list":
				http.Error(w, "boom", http.StatusInternalServerError)
			case "/news/bad.html":
				http.Error(w, "boom", http.StatusInternalServerError)
			case "/news/1.html":
				_, _ = w.Write([]byte(articlePage("第一篇新闻文章的完整中文标题", articleParas(10)...)))
			default:
				http.NotFound(w, r)
			}
		},
	})

	dir := t.TempDir()
	cfg := Config{
		Seeds:       []string{"http://" + hostA + "/"},
		Depth:       2,
		OutputDir:   dir,
		Mode:        ModeFull,
		Concurrency: 2,
		Fetch:       env.fetch,
	}

	stats, err := Collect(cfg)
	if err != nil {
		t.Fatalf("Collect should not return error for page failures: %v", err)
	}

	records := loadRecords(t, dir)
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1 (successful article retained)", len(records))
	}

	failures := loadFailures(t, dir)
	if len(failures) != 2 {
		t.Fatalf("failures = %d, want 2", len(failures))
	}

	stages := map[Stage]int{}
	descs := map[string]int{}
	for _, f := range failures {
		stages[f.Stage]++
		descs[f.Error]++
	}
	if stages[StageList] != 1 || stages[StageContent] != 1 {
		t.Fatalf("failure stages wrong: %+v", stages)
	}
	// 同类失败描述一致
	if descs[errDescListFetch] != 1 || descs[errDescContentFetch] != 1 {
		t.Fatalf("failure descriptions wrong: %+v", descs)
	}
	if stats.Failed != 2 || stats.Success != 1 {
		t.Fatalf("stats wrong: %+v", stats)
	}
	if stats.Planned != stats.Success+stats.Skipped+stats.Failed {
		t.Fatalf("identity broken: %+v", stats)
	}
}

// TestRunPerHostConcurrency 同一 host 并发不超过配置值, 不同 host 可并行。
func TestRunPerHostConcurrency(t *testing.T) {
	var totalInflight int32
	var maxTotalInflight int32
	releaseGate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(releaseGate) }) }
	t.Cleanup(release)

	makeHost := func(host string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if r.URL.Path == "/" {
				entries := make([][2]string, 0, 8)
				for i := 1; i <= 8; i++ {
					entries = append(entries, [2]string{
						"/news/" + itoa(i) + ".html",
						"第" + itoa(i) + "篇新闻文章的完整中文标题内容",
					})
				}
				_, _ = w.Write([]byte(listPage("首页列表", entries)))
				return
			}
			// 内容页先阻塞到放行, 以此压出真实并发; 带硬超时防止永久挂死
			cur := atomic.AddInt32(&totalInflight, 1)
			for {
				old := atomic.LoadInt32(&maxTotalInflight)
				if cur <= old || atomic.CompareAndSwapInt32(&maxTotalInflight, old, cur) {
					break
				}
			}
			select {
			case <-releaseGate:
			case <-time.After(3 * time.Second):
			}
			atomic.AddInt32(&totalInflight, -1)
			_, _ = w.Write([]byte(articlePage("新闻文章的完整中文标题内容", articleParas(10)...)))
		}
	}

	env := newTestEnv(t, map[string]http.HandlerFunc{
		hostA: makeHost(hostA),
		hostB: makeHost(hostB),
	})

	dir := t.TempDir()
	cfg := Config{
		Seeds:       []string{"http://" + hostA + "/", "http://" + hostB + "/"},
		Depth:       1,
		OutputDir:   dir,
		Mode:        ModeFull,
		Concurrency: 2,
		Fetch:       env.fetch,
	}

	var stats Stats
	var runErr error
	done := make(chan struct{})
	go func() {
		stats, runErr = Collect(cfg)
		close(done)
	}()

	// 等待每个 host 的并发都打满到上限 2
	waitUntil(t, func() bool {
		return env.maxConcurrent(hostA) >= 2 && env.maxConcurrent(hostB) >= 2
	}, 3*time.Second)

	if env.maxConcurrent(hostA) > 2 || env.maxConcurrent(hostB) > 2 {
		t.Fatalf("per-host concurrency exceeded: A=%d B=%d",
			env.maxConcurrent(hostA), env.maxConcurrent(hostB))
	}
	// 两个 host 各有 2 个在阻塞, 证明不同 host 之间确实并行
	if atomic.LoadInt32(&maxTotalInflight) < 4 {
		t.Fatalf("expected 4 concurrent requests across two hosts, got %d",
			atomic.LoadInt32(&maxTotalInflight))
	}

	// 放行全部阻塞请求
	release()

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("Collect did not finish after releasing blocked requests")
	}
	if runErr != nil {
		t.Fatalf("Collect: %v", runErr)
	}
	if stats.Success != 16 {
		t.Fatalf("success = %d, want 16", stats.Success)
	}
	if env.maxConcurrent(hostA) > 2 || env.maxConcurrent(hostB) > 2 {
		t.Fatalf("per-host concurrency exceeded: A=%d B=%d",
			env.maxConcurrent(hostA), env.maxConcurrent(hostB))
	}
}

// TestRunRelativeLinkResolveFailure 无法解析的相对链接记为列表阶段失败, 不拖垮整批。
func TestRunRelativeLinkResolveFailure(t *testing.T) {
	env := newTestEnv(t, map[string]http.HandlerFunc{
		hostA: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if r.URL.Path == "/" {
				// 含控制字符的 href, url.Parse 会失败
				_, _ = w.Write([]byte(`<!DOCTYPE html><html lang="zh"><head><title>首页列表页面</title></head><body>` +
					`<a href="http://` + hostA + `/news/1.html">第一篇新闻文章的完整中文标题内容</a>` +
					`<a href=":bad%zz">另一个链接的完整中文锚文本内容</a>` +
					`</body></html>`))
				return
			}
			_, _ = w.Write([]byte(articlePage("第一篇新闻文章的完整中文标题", articleParas(10)...)))
		},
	})

	dir := t.TempDir()
	stats, err := Collect(Config{
		Seeds:       []string{"http://" + hostA + "/"},
		Depth:       1,
		OutputDir:   dir,
		Mode:        ModeFull,
		Concurrency: 1,
		Fetch:       env.fetch,
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if stats.Success != 1 {
		t.Fatalf("success = %d, want 1", stats.Success)
	}
	failures := loadFailures(t, dir)
	if len(failures) != 1 || failures[0].Stage != StageList || failures[0].Error != errDescLinkResolve {
		t.Fatalf("expected one link-resolve failure, got %+v", failures)
	}
}

// TestRunConfigValidation 非法配置直接报错, 不产生输出目录垃圾。
func TestRunConfigValidation(t *testing.T) {
	env := twoLevelEnv(t)
	if _, err := New(Config{Seeds: nil, Depth: 1, OutputDir: t.TempDir(), Fetch: env.fetch}); err == nil {
		t.Fatal("expected error for empty seeds")
	}
	if _, err := New(Config{Seeds: []string{"http://" + hostA + "/"}, Depth: 0, OutputDir: t.TempDir(), Fetch: env.fetch}); err == nil {
		t.Fatal("expected error for zero depth")
	}
	if _, err := New(Config{Seeds: []string{"http://" + hostA + "/"}, Depth: 1, OutputDir: "", Fetch: env.fetch}); err == nil {
		t.Fatal("expected error for empty output dir")
	}
}

// waitUntil 轮询条件直到成立或超时。
func waitUntil(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

// itoa 避免引入 strconv 的局部小工具。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
