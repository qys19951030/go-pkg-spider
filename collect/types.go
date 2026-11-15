// Package collect 在现有链接分类与正文抽取能力之上, 提供可长期运行、
// 可增量补采、落盘可恢复的新闻采集管道。
package collect

// Mode 采集模式
type Mode int

const (
	// ModeFull 全量模式: 忽略 checkpoint 中的已完成集合, 重新遍历,
	// 但落库仍按规范化 URL 去重, 已有记录不覆盖、不重写。
	ModeFull Mode = iota
	// ModeIncremental 增量模式: 启动先读 checkpoint, 已完成的 URL 不再请求。
	ModeIncremental
)

// Stage 失败发生的阶段
type Stage string

const (
	// StageList 列表页阶段(列表页抓取/解析、链接解析)
	StageList Stage = "list"
	// StageContent 内容页阶段(内容页抓取/解析/抽取)
	StageContent Stage = "content"
)

// 统一的失败描述, 同一批里同类失败的描述保持一致, 不夹带 URL 等易变信息。
const (
	errDescListFetch      = "列表页抓取失败"
	errDescListParse      = "列表页解析失败"
	errDescContentFetch   = "内容页抓取失败"
	errDescContentParse   = "内容页解析失败"
	errDescContentExtract = "内容页抽取失败"
	errDescLinkResolve    = "链接解析为绝对地址失败"
)

// Record 一条结构化新闻记录, 字段顺序固定, JSON 序列化即落盘格式。
type Record struct {
	Url         string `json:"url"`
	Title       string `json:"title"`
	PublishTime string `json:"publish_time"`
	Lang        string `json:"lang"`
	Content     string `json:"content"`
	ListUrl     string `json:"list_url"`
	Depth       uint8  `json:"depth"`
	Seq         int64  `json:"seq"`
}

// Failure 一条失败条目。
type Failure struct {
	Url   string `json:"url"`
	Stage Stage  `json:"stage"`
	Error string `json:"error"`
}

// Checkpoint 断点文件, 与 records.jsonl 严格对齐。
type Checkpoint struct {
	// Completed 已完成(已落库)的规范化 URL, 升序排列
	Completed []string `json:"completed"`
	// Count 已落库条数, 等于 len(Completed) 与 records.jsonl 行数
	Count int64 `json:"count"`
	// Seeds 本任务的规范化种子地址, 升序排列
	Seeds []string `json:"seeds"`
	// Mode 采集模式
	Mode Mode `json:"mode"`
}

// Stats 一批采集结束后的计数。
// 恒等关系: Planned == Success + Skipped + Failed。
type Stats struct {
	// Planned 计划处理条目数: 去重后的内容候选数 + 全部失败条目数
	Planned int
	// Success 本批新写入 records.jsonl 的条数
	Success int
	// Skipped 跳过数: 本批重复发现、已有记录、增量模式已完成而未请求的条目
	Skipped int
	// Failed 失败条目数, 等于本批写入 failures.jsonl 的行数
	Failed int
}

// Config 采集配置。
type Config struct {
	// Seeds 种子地址
	Seeds []string
	// Depth 采集深度(按层遍历的层数, 至少为 1)
	Depth uint8
	// OutputDir 输出目录, 存放 records.jsonl / failures.jsonl / checkpoint.json
	OutputDir string
	// Mode 全量或增量
	Mode Mode
	// Concurrency 同一 host 上同时进行的请求数上限, <=0 时取默认值
	Concurrency int
	// Fetch 可替换的取页函数, 为 nil 时使用基于 spider.HttpGet 的默认实现
	Fetch FetchFunc
}

// contentCandidate 一个待抓取的内容页候选。
type contentCandidate struct {
	url     string
	listUrl string
	depth   uint8
}
