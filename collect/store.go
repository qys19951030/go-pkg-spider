package collect

import "os"

// Store 管理输出目录下的 records.jsonl、failures.jsonl、checkpoint.json,
// 保证 checkpoint 与记录严格对齐, 进程任意时刻被杀都不会重复追加同一篇。
type Store struct {
	dir          string
	records      map[string]Record // 已落库记录(含历史批次), key 为规范化 URL
	recordFile   *os.File
	failureFile  *os.File
	recordCount  int64
	failureCount int
}

// OpenStore 打开(或创建)输出目录, 载入历史 records.jsonl / checkpoint.json
// 并校验二者严格对齐。
func OpenStore(dir string, seeds []string, mode Mode) (*Store, error) {
	return nil, nil
}

// HasRecord 规范化 URL 是否已有落库记录。
func (s *Store) HasRecord(u string) bool {
	return false
}

// AppendFailures 追加一批失败条目(按 URL、阶段排序, 描述已统一)。
func (s *Store) AppendFailures(failures []Failure) error {
	return nil
}

// Commit 把本批新记录按规范化 URL 升序、seq 连续递增写入 records.jsonl,
// 再原子重写 checkpoint.json。返回新写入条数。
func (s *Store) Commit(newRecords []Record) (int, error) {
	return 0, nil
}

// Close 关闭底层文件句柄。
func (s *Store) Close() error {
	return nil
}
