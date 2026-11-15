package collect

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "小写 scheme/host, 去 www, 去锚点, 去末尾斜杠",
			in:   "HTTP://WWW.Example.COM/News/2026/#section-1",
			want: "http://example.com/News/2026",
		},
		{
			name: "显式 80/443 端口去掉",
			in:   "https://www.example.com:443/a",
			want: "https://example.com/a",
		},
		{
			name: "http 上的 443 也按规则去掉",
			in:   "http://example.com:443/a/",
			want: "http://example.com/a",
		},
		{
			name: "非常规端口保留",
			in:   "http://example.com:8080/a/",
			want: "http://example.com:8080/a",
		},
		{
			name: "根路径保留斜杠",
			in:   "http://example.com/",
			want: "http://example.com/",
		},
		{
			name: "连续斜杠合并",
			in:   "http://example.com//a///b//",
			want: "http://example.com/a/b",
		},
		{
			name: "utm 与跟踪参数全部去掉",
			in:   "http://example.com/a?utm_source=x&from=timeline&spm=1&share_token=abc&share_source=wx&clickfrom=link&id=9",
			want: "http://example.com/a?id=9",
		},
		{
			name: "剩余参数按名升序",
			in:   "http://example.com/a?z=1&a=2&m=3",
			want: "http://example.com/a?a=2&m=3&z=1",
		},
		{
			name: "同名参数只留第一个",
			in:   "http://example.com/a?b=1&b=2&a=3&a=4",
			want: "http://example.com/a?a=3&b=1",
		},
		{
			name: "参数全部被丢弃后无查询串",
			in:   "http://example.com/a?utm_x=1",
			want: "http://example.com/a",
		},
		{
			name: "去 www 只去前缀一次",
			in:   "http://www.www.example.com/a",
			want: "http://www.example.com/a",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize(tc.in)
			if err != nil {
				t.Fatalf("Normalize(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeUnsupported(t *testing.T) {
	for _, in := range []string{"ftp://example.com/a", "mailto:a@b.com", "not a url"} {
		if _, err := Normalize(in); err == nil {
			t.Fatalf("Normalize(%q) expected error", in)
		}
	}
}

func TestResolveNormalize(t *testing.T) {
	base := "http://www.example.com/list/index.html?from=x"

	got, err := ResolveNormalize("../news/1.html?utm_source=s#frag", base)
	if err != nil {
		t.Fatalf("ResolveNormalize error: %v", err)
	}
	want := "http://example.com/news/1.html"
	if got != want {
		t.Fatalf("ResolveNormalize = %q, want %q", got, want)
	}

	// 绝对地址按自身规范化
	got, err = ResolveNormalize("HTTPS://WWW.example.com/A///", base)
	if err != nil {
		t.Fatalf("ResolveNormalize error: %v", err)
	}
	if got != "https://example.com/A" {
		t.Fatalf("ResolveNormalize = %q, want %q", got, "https://example.com/A")
	}

	// 无法解析的 base 直接失败
	if _, err := ResolveNormalize("/a", "://bad base"); err == nil {
		t.Fatal("ResolveNormalize expected error for bad base")
	}
}

func TestSameDomain(t *testing.T) {
	if !sameDomain("http://www.example.com/a", "http://news.example.com/b") {
		t.Fatal("subdomains of same top domain should be same domain")
	}
	if sameDomain("http://example.com/a", "http://other.org/b") {
		t.Fatal("different top domains should not be same domain")
	}
}
