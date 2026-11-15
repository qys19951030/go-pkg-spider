package collect

// FetchFunc 可替换的取页动作: 给定 URL 返回页面字节。
// 默认实现 defaultFetch 走 spider.HttpGet; 测试中替换为 httptest 本地站点。
type FetchFunc func(url string) ([]byte, error)

// defaultFetch 基于 spider.HttpGet 的默认取页实现。
func defaultFetch(url string) ([]byte, error) {
	return nil, nil
}
