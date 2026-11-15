package collect

import (
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/suosi-inc/go-pkg-spider/extract"
)

var (
	// 路径中连续的斜杠
	multiSlashPattern = regexp.MustCompile(`/+`)
	// 需要丢弃的查询参数名(精确匹配)
	dropQueryParams = map[string]bool{
		"from":         true,
		"spm":          true,
		"share_token":  true,
		"share_source": true,
		"clickfrom":    true,
	}
)

// Normalize 规范化一个绝对 URL。
// 规则:
//  1. scheme 与 host 统一小写, host 开头的 www 去掉,
//     显式写出的 80 和 443 端口去掉, 锚点整段去掉。
//  2. 路径末尾多余的斜杠去掉(根路径保留), 路径中连续的斜杠合并成一个。
//  3. 查询参数去掉 utm_ 前缀参数及 from、spm、share_token、share_source、
//     clickfrom, 剩余按参数名升序, 同名参数只留第一个。
func Normalize(rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", err
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("normalize: unsupported scheme")
	}

	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", errors.New("normalize: empty host")
	}

	// host 开头的 www 去掉(只去一个标签前缀)
	host = strings.TrimPrefix(host, "www.")

	// 显式写出的 80 和 443 端口去掉, 其余端口保留
	port := u.Port()
	if port == "80" || port == "443" {
		port = ""
	}

	// 路径: 连续斜杠合并, 末尾斜杠去掉(根路径保留)
	p := multiSlashPattern.ReplaceAllString(u.Path, "/")
	if p == "" {
		p = "/"
	}
	if p != "/" {
		p = strings.TrimRight(p, "/")
		if p == "" {
			p = "/"
		}
	}

	// 查询参数清洗
	rawQuery := normalizeQuery(u.RawQuery)

	nu := &url.URL{
		Scheme:   scheme,
		Host:     host,
		Path:     p,
		RawQuery: rawQuery,
	}
	if port != "" {
		nu.Host = host + ":" + port
	}

	return nu.String(), nil
}

// normalizeQuery 去掉跟踪参数, 同名只留第一个, 按参数名升序输出。
func normalizeQuery(raw string) string {
	if raw == "" {
		return ""
	}

	first := map[string]string{}
	order := make([]string, 0)
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, "=", 2)
		key := kv[0]
		if key == "" {
			continue
		}
		// 丢弃 utm_ 前缀参数与指定跟踪参数
		if strings.HasPrefix(key, "utm_") || dropQueryParams[key] {
			continue
		}
		if _, exists := first[key]; exists {
			continue
		}
		val := ""
		if len(kv) == 2 {
			val = kv[1]
		}
		first[key] = val
		order = append(order, key)
	}

	if len(order) == 0 {
		return ""
	}

	sort.Strings(order)

	var b strings.Builder
	for i, key := range order {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(key))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(first[key]))
	}
	return b.String()
}

// ResolveNormalize 按所在列表页 base 把相对链接解析成绝对地址后再规范化。
// 解析失败返回错误, 由调用方记为失败条目。
func ResolveNormalize(href string, base string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}

	ref, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return "", err
	}

	resolved := baseURL.ResolveReference(ref)
	if resolved.Host == "" {
		return "", errors.New("normalize: resolved url has empty host")
	}

	return Normalize(resolved.String())
}

// sameDomain 判断两个规范化 URL 是否属于同一顶级域名(同域才跟进)。
func sameDomain(a string, b string) bool {
	ua, err := url.Parse(a)
	if err != nil {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil {
		return false
	}

	da := extract.DomainTop(ua.Hostname())
	db := extract.DomainTop(ub.Hostname())
	if da == "" || db == "" {
		return false
	}
	return da == db
}
