package collect

import (
	"github.com/suosi-inc/go-pkg-spider"
)

// FetchFunc 可替换的取页动作: 给定 URL 返回页面字节。
// 默认实现 defaultFetch 走 spider.HttpGet; 测试中替换为 httptest 本地站点,
// collect 包的用例一律不访问外网。
type FetchFunc func(url string) ([]byte, error)

// defaultFetch 基于 spider.HttpGet 的默认取页实现, 复用现有的请求、
// 重试之外的 HTTP 能力(字符集探测与转码由 HttpGet 内部完成)。
func defaultFetch(u string) ([]byte, error) {
	body, err := spider.HttpGet(u)
	if err != nil {
		return nil, err
	}
	return body, nil
}
