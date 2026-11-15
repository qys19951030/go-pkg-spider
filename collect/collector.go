package collect

import (
	"bytes"
	"fmt"
	"net/url"
	"sort"
	"sync"

	"github.com/PuerkitoBio/goquery"
	"github.com/suosi-inc/go-pkg-spider"
	"github.com/suosi-inc/go-pkg-spider/extract"
)

// defaultConcurrency 同一 host 默认并发上限
const defaultConcurrency = 2

// Collector 采集器, 复用现有链接分类与正文抽取, 按 host 限流并发采集。
type Collector struct {
	cfg   Config
	store *Store
	fetch FetchFunc

	slotsMu sync.Mutex
	slots   map[string]chan struct{}
}

// New 按配置构造采集器, 校验配置并规范化种子。
func New(cfg Config) (*Collector, error) {
	if len(cfg.Seeds) == 0 {
		return nil, fmt.Errorf("collect: empty seeds")
	}
	if cfg.Depth == 0 {
		return nil, fmt.Errorf("collect: depth must be >= 1")
	}
	if cfg.OutputDir == "" {
		return nil, fmt.Errorf("collect: empty output dir")
	}

	normalized := make([]string, 0, len(cfg.Seeds))
	seenSeed := make(map[string]bool, len(cfg.Seeds))
	for _, seed := range cfg.Seeds {
		n, err := Normalize(seed)
		if err != nil {
			return nil, fmt.Errorf("collect: normalize seed %q: %w", seed, err)
		}
		if !seenSeed[n] {
			seenSeed[n] = true
			normalized = append(normalized, n)
		}
	}
	sort.Strings(normalized)
	cfg.Seeds = normalized

	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaultConcurrency
	}
	if cfg.Fetch == nil {
		cfg.Fetch = defaultFetch
	}

	store, err := OpenStore(cfg.OutputDir, cfg.Seeds, cfg.Mode)
	if err != nil {
		return nil, err
	}

	return &Collector{
		cfg:   cfg,
		store: store,
		fetch: cfg.Fetch,
		slots: make(map[string]chan struct{}),
	}, nil
}

// Collect 是包级便捷入口: 构造采集器并执行一批采集。
func Collect(cfg Config) (Stats, error) {
	c, err := New(cfg)
	if err != nil {
		return Stats{}, err
	}
	defer c.Close()
	return c.Run()
}

// Close 释放底层资源。
func (c *Collector) Close() error {
	if c.store != nil {
		return c.store.Close()
	}
	return nil
}

// hostSlot 返回某个 host 的并发信号量(同 host 并发不超过配置值)。
// 不同 host 各自持有独立信号量, 因此不同 host 之间可以并行。
func (c *Collector) hostSlot(host string) chan struct{} {
	c.slotsMu.Lock()
	defer c.slotsMu.Unlock()

	slot, ok := c.slots[host]
	if !ok {
		slot = make(chan struct{}, c.cfg.Concurrency)
		c.slots[host] = slot
	}
	return slot
}

// runParallel 按 host 限流并发执行任务, 收集结果。
// 派发在独立 goroutine 中进行, host 信号量在 worker 内获取:
// 某个 host 的槽位打满只会阻塞该 host 的任务, 不影响其他 host 并行。
func runParallel[In any, Out any](c *Collector, items []In, hostFn func(In) string, work func(In) Out) []Out {
	results := make([]Out, len(items))

	type job struct {
		idx int
		in  In
	}
	jobs := make(chan job)
	var wg sync.WaitGroup

	// worker 数量不设全局上限: 真正的并发约束由每个 host 的信号量保证,
	// 不同 host 之间可以自由并行。
	workers := len(items)
	if workers < 1 {
		workers = 1
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				host := hostFn(j.in)
				slot := c.hostSlot(host)
				slot <- struct{}{}
				results[j.idx] = work(j.in)
				<-slot
			}
		}()
	}

	go func() {
		for i, item := range items {
			jobs <- job{idx: i, in: item}
		}
		close(jobs)
	}()

	wg.Wait()
	return results
}

// hostOfURL 取 URL 的 host, 用于按 host 限流。
func hostOfURL(u string) string {
	if parsed, err := url.Parse(u); err == nil {
		return parsed.Hostname()
	}
	return ""
}

// hostOfCandidate 取内容候选 URL 的 host。
func hostOfCandidate(cand contentCandidate) string {
	return hostOfURL(cand.url)
}

// Run 执行一批采集, 返回计数。单个页面失败不拖垮整批。
func (c *Collector) Run() (Stats, error) {
	var stats Stats

	// seen 记录本进程已发现(已入队或已落库)的规范化 URL, 同 URL 只处理第一次
	seen := make(map[string]bool)

	// 增量模式: 已完成的 URL 不再请求, 直接计入跳过
	completed := make(map[string]bool)
	if c.cfg.Mode == ModeIncremental {
		for _, r := range c.store.records {
			completed[r.Url] = true
		}
	}

	frontier := make([]string, 0, len(c.cfg.Seeds))
	for _, seed := range c.cfg.Seeds {
		if !seen[seed] {
			seen[seed] = true
			frontier = append(frontier, seed)
		}
	}

	// 按深度逐层遍历列表页
	for depth := uint8(1); depth <= c.cfg.Depth && len(frontier) > 0; depth++ {
		current := frontier
		frontier = nil

		// 增量模式: 已完成的列表页不请求
		pending := make([]string, 0, len(current))
		for _, u := range current {
			if completed[u] {
				stats.Skipped++
				continue
			}
			pending = append(pending, u)
		}

		listResults := runParallel(c, pending, hostOfURL, func(listURL string) listResult {
			l, cand, fails := c.fetchList(listURL, depth)
			return listResult{listURLs: l, candidates: cand, failures: fails}
		})

		var candidates []contentCandidate
		for _, res := range listResults {
			if err := c.store.AppendFailures(res.failures); err != nil {
				return stats, err
			}
			stats.Failed += len(res.failures)

			// 下一层列表页(最后一层不再入队)
			if depth < c.cfg.Depth {
				for _, u := range res.listURLs {
					if !seen[u] {
						seen[u] = true
						frontier = append(frontier, u)
					}
				}
			}

			// 内容页候选: 同 URL 保留第一次发现时的 list_url 与 depth
			for _, cand := range res.candidates {
				if !seen[cand.url] {
					seen[cand.url] = true
					candidates = append(candidates, cand)
				} else {
					stats.Skipped++
				}
			}
		}

		if len(candidates) == 0 {
			continue
		}

		// 全量模式: 已有记录的内容页仍重新请求, 但落库时跳过;
		// 增量模式: 已完成的内容页直接跳过, 不再请求
		toFetch := make([]contentCandidate, 0, len(candidates))
		for _, cand := range candidates {
			if completed[cand.url] {
				stats.Skipped++
				continue
			}
			if c.cfg.Mode == ModeFull && c.store.HasRecord(cand.url) {
				stats.Skipped++
				continue
			}
			toFetch = append(toFetch, cand)
		}

		contentResults := runParallel(c, toFetch, hostOfCandidate, c.fetchContent)

		var newRecords []Record
		var contentFailures []Failure
		for _, cr := range contentResults {
			if cr.failure != nil {
				contentFailures = append(contentFailures, *cr.failure)
				continue
			}
			if cr.record != nil {
				newRecords = append(newRecords, *cr.record)
			}
		}

		if err := c.store.AppendFailures(contentFailures); err != nil {
			return stats, err
		}
		stats.Failed += len(contentFailures)

		written, err := c.store.Commit(newRecords)
		if err != nil {
			return stats, err
		}
		stats.Success += written
		// Commit 内部仍会防御性去重, 被去重的计入跳过
		stats.Skipped += len(newRecords) - written
	}

	// 计数恒等式: Planned = Success + Skipped + Failed
	stats.Planned = stats.Success + stats.Skipped + stats.Failed
	return stats, nil
}

// listResult 单个列表页的处理结果。
type listResult struct {
	listURLs   []string
	candidates []contentCandidate
	failures   []Failure
}

// fetchList 抓取并解析一个列表页, 返回同域的列表/内容链接及解析失败条目。
// 列表页复用 extract.WebLinkTitles 与 extract.LinkTypes 做链接提取与分类,
// 语种探测复用根包 Lang。
func (c *Collector) fetchList(listURL string, depth uint8) (listURLs []string, candidates []contentCandidate, failures []Failure) {
	body, err := c.fetch(listURL)
	if err != nil {
		return nil, nil, []Failure{{Url: listURL, Stage: StageList, Error: errDescListFetch}}
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, nil, []Failure{{Url: listURL, Stage: StageList, Error: errDescListParse}}
	}
	doc.Find(spider.DefaultDocRemoveTags).Remove()

	baseURL, err := url.Parse(listURL)
	if err != nil {
		return nil, nil, []Failure{{Url: listURL, Stage: StageList, Error: errDescListParse}}
	}

	langRes := spider.Lang(doc, "", true)

	// 复用现有的链接提取与分类; 无法解析为绝对地址的链接进入 filters,
	// 按要求直接记为列表阶段的失败条目
	linkTitles, filters := extract.WebLinkTitles(doc, baseURL, false)
	for raw := range filters {
		failures = append(failures, Failure{Url: raw, Stage: StageList, Error: errDescLinkResolve})
	}
	linkRes, _ := extract.LinkTypes(linkTitles, langRes.Lang, nil)

	classified := make(map[string]string, len(linkRes.Content)+len(linkRes.List))
	// 同一 URL 同时被分到两类时, 内容页优先
	for u, title := range linkRes.List {
		classified[u] = title
	}
	for u, title := range linkRes.Content {
		classified[u] = title
	}

	// 确定性遍历
	rawURLs := make([]string, 0, len(classified))
	for u := range classified {
		rawURLs = append(rawURLs, u)
	}
	sort.Strings(rawURLs)

	contentSet := linkRes.Content

	for _, raw := range rawURLs {
		normalized, nerr := ResolveNormalize(raw, listURL)
		if nerr != nil {
			failures = append(failures, Failure{Url: raw, Stage: StageList, Error: errDescLinkResolve})
			continue
		}
		// 只跟进同域的列表页与内容页
		if !sameDomain(normalized, listURL) {
			continue
		}
		if _, isContent := contentSet[raw]; isContent {
			candidates = append(candidates, contentCandidate{
				url:     normalized,
				listUrl: listURL,
				depth:   depth,
			})
		} else {
			listURLs = append(listURLs, normalized)
		}
	}

	return listURLs, candidates, failures
}

// contentResult 单个内容页的处理结果。
type contentResult struct {
	record  *Record
	failure *Failure
}

// fetchContent 抓取并抽取一个内容页, 返回记录或失败条目。
// 语种探测复用根包 Lang, 正文抽取复用 extract.NewContent。
func (c *Collector) fetchContent(cand contentCandidate) contentResult {
	body, err := c.fetch(cand.url)
	if err != nil {
		return contentResult{failure: &Failure{Url: cand.url, Stage: StageContent, Error: errDescContentFetch}}
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return contentResult{failure: &Failure{Url: cand.url, Stage: StageContent, Error: errDescContentParse}}
	}

	contentDoc := goquery.CloneDocument(doc)
	doc.Find(spider.DefaultDocRemoveTags).Remove()

	langRes := spider.Lang(doc, "", false)
	content := extract.NewContent(contentDoc, langRes.Lang, "", cand.url)
	news := content.ExtractNews()
	if news == nil {
		return contentResult{failure: &Failure{Url: cand.url, Stage: StageContent, Error: errDescContentExtract}}
	}

	return contentResult{record: &Record{
		Url:         cand.url,
		Title:       news.Title,
		PublishTime: news.TimeLocal,
		Lang:        news.Lang,
		Content:     news.Content,
		ListUrl:     cand.listUrl,
		Depth:       cand.depth,
	}}
}
