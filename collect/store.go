package collect

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const (
	recordsFile    = "records.jsonl"
	failuresFile   = "failures.jsonl"
	checkpointFile = "checkpoint.json"
)

// Store 管理输出目录下的 records.jsonl、failures.jsonl、checkpoint.json,
// 保证 checkpoint 与记录严格对齐, 进程任意时刻被杀都不会重复追加同一篇。
type Store struct {
	dir string

	mu sync.Mutex

	// records 已落库记录(含历史批次), key 为规范化 URL
	records map[string]Record

	recordFile  *os.File
	failureFile *os.File

	// recordCount 已落库条数, 等于 len(records) 与 records.jsonl 有效行数
	recordCount int64
	// failureCount 本进程累计写入的失败条数
	failureCount int

	seeds []string
	mode  Mode
}

// OpenStore 打开(或创建)输出目录, 载入历史 records.jsonl / checkpoint.json
// 并校验二者严格对齐: checkpoint 标记完成的 URL 必须都有对应记录,
// 否则返回错误; checkpoint 落后于记录(写入后被杀)则按记录重建。
func OpenStore(dir string, seeds []string, mode Mode) (*Store, error) {
	if dir == "" {
		return nil, errors.New("store: empty output dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: create output dir: %w", err)
	}

	s := &Store{
		dir:     dir,
		records: make(map[string]Record),
		seeds:   seeds,
		mode:    mode,
	}

	recordsPath := filepath.Join(dir, recordsFile)

	validEnd, partial, err := s.loadRecords(recordsPath)
	if err != nil {
		return nil, err
	}

	// 上次进程在写入最后一行时被杀, 截断半截行后再以追加方式打开
	if partial {
		if err := os.Truncate(recordsPath, validEnd); err != nil {
			return nil, fmt.Errorf("store: truncate partial record line: %w", err)
		}
	}

	if err := s.loadAndVerifyCheckpoint(); err != nil {
		return nil, err
	}

	s.recordFile, err = os.OpenFile(filepath.Join(dir, recordsFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("store: open records file: %w", err)
	}
	s.failureFile, err = os.OpenFile(filepath.Join(dir, failuresFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("store: open failures file: %w", err)
	}

	return s, nil
}

// loadRecords 逐行载入历史记录, 返回最后一个完整行之后的字节偏移。
// 遇到半截行(反序列化失败)视为上次崩溃残留, 停止载入并交由调用方截断。
// 第二个返回值标识是否存在需要截断的半截行。
func (s *Store) loadRecords(path string) (int64, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("store: open records for load: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)

	var offset int64
	for scanner.Scan() {
		line := scanner.Bytes()
		lineEnd := offset + int64(len(line)) + 1 // 含换行符

		if len(bytes.TrimSpace(line)) == 0 {
			offset = lineEnd
			continue
		}

		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			// 半截行, 保留 offset 作为截断点
			return offset, true, nil
		}
		if r.Url == "" {
			return offset, true, nil
		}
		s.records[r.Url] = r
		s.recordCount++
		offset = lineEnd
	}
	if err := scanner.Err(); err != nil {
		return 0, false, fmt.Errorf("store: scan records: %w", err)
	}
	return offset, false, nil
}

// loadAndVerifyCheckpoint 校验 checkpoint 不允许出现标了完成却没有对应记录的情况。
func (s *Store) loadAndVerifyCheckpoint() error {
	path := filepath.Join(s.dir, checkpointFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("store: read checkpoint: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}

	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return fmt.Errorf("store: parse checkpoint: %w", err)
	}

	for _, u := range cp.Completed {
		if _, ok := s.records[u]; !ok {
			return fmt.Errorf("store: checkpoint marks %q completed but no record found", u)
		}
	}
	return nil
}

// HasRecord 规范化 URL 是否已有落库记录。
func (s *Store) HasRecord(u string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.records[u]
	return ok
}

// RecordCount 已落库条数。
func (s *Store) RecordCount() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordCount
}

// AppendFailures 追加一批失败条目(按 URL、阶段排序, 描述已统一)。
// 失败条目独立于记录与 checkpoint, 追加写不影响采集可恢复性。
func (s *Store) AppendFailures(failures []Failure) error {
	if len(failures) == 0 {
		return nil
	}

	sorted := make([]Failure, len(failures))
	copy(sorted, failures)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Url != sorted[j].Url {
			return sorted[i].Url < sorted[j].Url
		}
		return sorted[i].Stage < sorted[j].Stage
	})

	s.mu.Lock()
	defer s.mu.Unlock()

	w := bufio.NewWriter(s.failureFile)
	for _, f := range sorted {
		line, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("store: marshal failure: %w", err)
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return fmt.Errorf("store: write failure: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("store: flush failures: %w", err)
	}
	if err := s.failureFile.Sync(); err != nil {
		return fmt.Errorf("store: sync failures: %w", err)
	}
	s.failureCount += len(sorted)
	return nil
}

// Commit 把本批新记录按规范化 URL 升序、seq 连续递增写入 records.jsonl,
// 再原子重写 checkpoint.json。返回新写入条数。
// 先落记录后落 checkpoint, 任意时刻崩溃重启都不会重复追加同一篇。
func (s *Store) Commit(newRecords []Record) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 去重防御: 已有记录一律不写
	unique := make([]Record, 0, len(newRecords))
	seenInBatch := make(map[string]bool, len(newRecords))
	for _, r := range newRecords {
		if _, exists := s.records[r.Url]; exists || seenInBatch[r.Url] {
			continue
		}
		seenInBatch[r.Url] = true
		unique = append(unique, r)
	}

	sort.Slice(unique, func(i, j int) bool { return unique[i].Url < unique[j].Url })

	w := bufio.NewWriter(s.recordFile)
	written := make([]Record, 0, len(unique))
	nextSeq := s.recordCount + 1
	for _, r := range unique {
		r.Seq = nextSeq
		nextSeq++

		line, err := json.Marshal(r)
		if err != nil {
			return len(written), fmt.Errorf("store: marshal record: %w", err)
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return len(written), fmt.Errorf("store: write record: %w", err)
		}
		written = append(written, r)
	}
	if err := w.Flush(); err != nil {
		return len(written), fmt.Errorf("store: flush records: %w", err)
	}
	if err := s.recordFile.Sync(); err != nil {
		return len(written), fmt.Errorf("store: sync records: %w", err)
	}

	for _, r := range written {
		s.records[r.Url] = r
	}
	s.recordCount += int64(len(written))

	if err := s.writeCheckpoint(); err != nil {
		return len(written), err
	}
	return len(written), nil
}

// writeCheckpoint 以临时文件 + rename 原子重写 checkpoint.json。
func (s *Store) writeCheckpoint() error {
	completed := make([]string, 0, len(s.records))
	for u := range s.records {
		completed = append(completed, u)
	}
	sort.Strings(completed)

	cp := Checkpoint{
		Completed: completed,
		Count:     s.recordCount,
		Seeds:     s.seeds,
		Mode:      s.mode,
	}
	data, err := json.MarshalIndent(&cp, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal checkpoint: %w", err)
	}
	data = append(data, '\n')

	tmp := filepath.Join(s.dir, checkpointFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("store: write checkpoint tmp: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, checkpointFile)); err != nil {
		return fmt.Errorf("store: rename checkpoint: %w", err)
	}
	return nil
}

// FailureCount 本进程写入的失败条数。
func (s *Store) FailureCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failureCount
}

// Close 关闭底层文件句柄。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var err error
	if s.recordFile != nil {
		err = errors.Join(err, s.recordFile.Close())
	}
	if s.failureFile != nil {
		err = errors.Join(err, s.failureFile.Close())
	}
	return err
}
