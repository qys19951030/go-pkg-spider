package collect

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func sampleRecords(urls ...string) []Record {
	out := make([]Record, 0, len(urls))
	for _, u := range urls {
		out = append(out, Record{
			Url:         u,
			Title:       "标题-" + u,
			PublishTime: "2026-01-01 10:00:00",
			Lang:        "zh",
			Content:     "正文内容",
			ListUrl:     "http://example.com/list",
			Depth:       1,
		})
	}
	return out
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return lines
}

func TestStoreCommitDeterministicAndSeq(t *testing.T) {
	dir := t.TempDir()

	st, err := OpenStore(dir, []string{"http://example.com/"}, ModeFull)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	// 故意乱序提交, 落盘必须按 URL 升序、seq 从 1 连续
	n, err := st.Commit(sampleRecords(
		"http://example.com/c/3",
		"http://example.com/c/1",
		"http://example.com/c/2",
	))
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n != 3 {
		t.Fatalf("Commit written = %d, want 3", n)
	}

	first := readLines(t, filepath.Join(dir, recordsFile))
	if len(first) != 3 {
		t.Fatalf("records lines = %d, want 3", len(first))
	}

	var got []Record
	for _, line := range first {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("unmarshal record: %v", err)
		}
		got = append(got, r)
	}
	wantOrder := []string{
		"http://example.com/c/1",
		"http://example.com/c/2",
		"http://example.com/c/3",
	}
	for i, r := range got {
		if r.Url != wantOrder[i] {
			t.Fatalf("record[%d] url = %q, want %q", i, r.Url, wantOrder[i])
		}
		if r.Seq != int64(i+1) {
			t.Fatalf("record[%d] seq = %d, want %d", i, r.Seq, i+1)
		}
	}

	// 第二批追加: 块内有序, seq 接续
	n, err = st.Commit(sampleRecords(
		"http://example.com/c/5",
		"http://example.com/c/4",
	))
	if err != nil {
		t.Fatalf("Commit 2: %v", err)
	}
	if n != 2 {
		t.Fatalf("Commit 2 written = %d, want 2", n)
	}

	all := readLines(t, filepath.Join(dir, recordsFile))
	if len(all) != 5 {
		t.Fatalf("records lines = %d, want 5", len(all))
	}
	for i, line := range all {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if r.Seq != int64(i+1) {
			t.Fatalf("seq gap at line %d: %d", i, r.Seq)
		}
	}

	// 已有记录不覆盖也不重写
	n, err = st.Commit(sampleRecords("http://example.com/c/1"))
	if err != nil {
		t.Fatalf("Commit dup: %v", err)
	}
	if n != 0 {
		t.Fatalf("dup Commit written = %d, want 0", n)
	}
	if lines := readLines(t, filepath.Join(dir, recordsFile)); len(lines) != 5 {
		t.Fatalf("records lines after dup = %d, want 5", len(lines))
	}

	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// checkpoint 与记录对得上
	cpData, err := os.ReadFile(filepath.Join(dir, checkpointFile))
	if err != nil {
		t.Fatalf("read checkpoint: %v", err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(cpData, &cp); err != nil {
		t.Fatalf("unmarshal checkpoint: %v", err)
	}
	if cp.Count != 5 || len(cp.Completed) != 5 {
		t.Fatalf("checkpoint count=%d completed=%d, want 5/5", cp.Count, len(cp.Completed))
	}
	if !sort.StringsAreSorted(cp.Completed) {
		t.Fatal("checkpoint completed not sorted")
	}

	// 重新打开, 历史记录仍在, 且不重复追加
	st2, err := OpenStore(dir, []string{"http://example.com/"}, ModeIncremental)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if st2.RecordCount() != 5 {
		t.Fatalf("record count after reopen = %d, want 5", st2.RecordCount())
	}
	if !st2.HasRecord("http://example.com/c/2") {
		t.Fatal("HasRecord should be true after reopen")
	}
	n, _ = st2.Commit(sampleRecords("http://example.com/c/2", "http://example.com/c/6"))
	if n != 1 {
		t.Fatalf("Commit after reopen written = %d, want 1", n)
	}
	lines := readLines(t, filepath.Join(dir, recordsFile))
	if len(lines) != 6 {
		t.Fatalf("records lines = %d, want 6", len(lines))
	}
	var r6 Record
	json.Unmarshal([]byte(lines[5]), &r6)
	if r6.Seq != 6 {
		t.Fatalf("last seq = %d, want 6", r6.Seq)
	}
	st2.Close()
}

func TestStoreByteIdenticalAcrossRuns(t *testing.T) {
	// 同样的新记录分两个独立目录各跑一遍, records.jsonl 逐字节一致
	recs := sampleRecords("http://example.com/c/3", "http://example.com/c/1", "http://example.com/c/2")

	dirA := t.TempDir()
	stA, _ := OpenStore(dirA, nil, ModeFull)
	if _, err := stA.Commit(recs); err != nil {
		t.Fatal(err)
	}
	stA.Close()

	dirB := t.TempDir()
	stB, _ := OpenStore(dirB, nil, ModeFull)
	if _, err := stB.Commit(recs); err != nil {
		t.Fatal(err)
	}
	stB.Close()

	a, _ := os.ReadFile(filepath.Join(dirA, recordsFile))
	b, _ := os.ReadFile(filepath.Join(dirB, recordsFile))
	if !bytesEqual(a, b) {
		t.Fatalf("records.jsonl not byte-identical:\n%s\n---\n%s", a, b)
	}
}

func TestStoreRecoverPartialLastLine(t *testing.T) {
	dir := t.TempDir()
	st, _ := OpenStore(dir, nil, ModeFull)
	if _, err := st.Commit(sampleRecords("http://example.com/c/1", "http://example.com/c/2")); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// 模拟进程在写最后一行时被杀: 追加半截 JSON(无换行)
	path := filepath.Join(dir, recordsFile)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"url":"http://example.com/c/3","tit`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	st2, err := OpenStore(dir, nil, ModeFull)
	if err != nil {
		t.Fatalf("reopen after partial line: %v", err)
	}
	if st2.RecordCount() != 2 {
		t.Fatalf("record count = %d, want 2 (partial line discarded)", st2.RecordCount())
	}
	if _, err := st2.Commit(sampleRecords("http://example.com/c/3")); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, filepath.Join(dir, recordsFile))
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	urls := map[string]bool{}
	for _, line := range lines {
		var r Record
		json.Unmarshal([]byte(line), &r)
		if urls[r.Url] {
			t.Fatalf("duplicate url after recovery: %s", r.Url)
		}
		urls[r.Url] = true
	}
	st2.Close()
}

func TestStoreCheckpointWithoutRecordFails(t *testing.T) {
	dir := t.TempDir()
	cp := Checkpoint{
		Completed: []string{"http://example.com/ghost"},
		Count:     1,
	}
	data, _ := json.Marshal(&cp)
	if err := os.WriteFile(filepath.Join(dir, checkpointFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(dir, nil, ModeIncremental); err == nil {
		t.Fatal("expected error when checkpoint marks url completed without record")
	}
}

func TestStoreAppendFailures(t *testing.T) {
	dir := t.TempDir()
	st, _ := OpenStore(dir, nil, ModeFull)

	err := st.AppendFailures([]Failure{
		{Url: "http://example.com/c/2", Stage: StageContent, Error: errDescContentFetch},
		{Url: "http://example.com/a/1", Stage: StageList, Error: errDescListFetch},
	})
	if err != nil {
		t.Fatalf("AppendFailures: %v", err)
	}
	if st.FailureCount() != 2 {
		t.Fatalf("failure count = %d, want 2", st.FailureCount())
	}
	st.Close()

	lines := readLines(t, filepath.Join(dir, failuresFile))
	if len(lines) != 2 {
		t.Fatalf("failure lines = %d, want 2", len(lines))
	}
	// 按 URL 排序, a/1 在 c/2 之前
	if !strings.Contains(lines[0], "http://example.com/a/1") {
		t.Fatalf("failures not sorted: %s", lines[0])
	}
	// 描述与阶段固定
	var f0 Failure
	json.Unmarshal([]byte(lines[0]), &f0)
	if f0.Stage != StageList || f0.Error != errDescListFetch {
		t.Fatalf("failure entry wrong: %+v", f0)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
