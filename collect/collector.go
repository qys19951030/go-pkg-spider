package collect

import "sync"

// Collector 采集器, 复用现有链接分类与正文抽取, 按 host 限流并发采集。
type Collector struct {
	cfg     Config
	store   *Store
	fetch   FetchFunc
	slotsMu sync.Mutex
	slots   map[string]chan struct{}
}

// New 按配置构造采集器, 校验配置并规范化种子。
func New(cfg Config) (*Collector, error) {
	return nil, nil
}

// Run 执行一批采集, 返回计数。单个页面失败不拖垮整批。
func (c *Collector) Run() (Stats, error) {
	return Stats{}, nil
}

// Collect 是包级便捷入口: 构造采集器并执行一批采集。
func Collect(cfg Config) (Stats, error) {
	return Stats{}, nil
}

// hostSlot 返回某个 host 的并发信号量(同 host 并发不超过配置值)。
func (c *Collector) hostSlot(host string) chan struct{} {
	return nil
}

// fetchList 抓取并解析一个列表页, 返回同域的列表/内容链接及解析失败条目。
func (c *Collector) fetchList(listURL string, depth uint8) (listURLs []string, candidates []contentCandidate, failures []Failure) {
	return nil, nil, nil
}

// fetchContent 抓取并抽取一个内容页, 返回记录或失败条目。
func (c *Collector) fetchContent(cand contentCandidate) (*Record, *Failure) {
	return nil, nil
}
