package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "64:ff9b::/96", "2002::/16"} {
		if netip.MustParsePrefix(prefix).Contains(ip) {
			return false
		}
	}
	return true
}
func publicURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || len(raw) > 4096 || u.Scheme != "https" && u.Scheme != "http" || u.Hostname() == "" || u.User != nil {
		return nil, userError("请填写公开网页或图片的 http(s) 地址")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !publicIP(ip) {
		return nil, userError("网页和图片地址不能指向本机、内网或保留地址")
	}
	u.Fragment = ""
	return u, nil
}
func publicWebClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second, MaxIdleConns: 8, IdleConnTimeout: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, userError("无法解析网页地址")
		}
		for _, ip := range ips {
			if !publicIP(ip) {
				return nil, userError("拒绝访问本机、内网或保留地址")
			}
		}
		var last error
		for _, ip := range ips {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			last = err
		}
		return nil, last
	}
	return &http.Client{Timeout: 20 * time.Second, Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return userError("网页重定向次数过多")
		}
		_, err := publicURL(req.URL.String())
		return err
	}}
}
func (s *Store) fetchPublic(ctx context.Context, raw, referer string, limit int64) ([]byte, string, string, error) {
	u, err := publicURL(raw)
	if err != nil {
		return nil, "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; AI-NFO/1.0)")
	req.Header.Set("Accept", "text/html,image/jpeg,image/png,image/webp;q=0.9,*/*;q=0.5")
	if ref, err := publicURL(referer); err == nil {
		req.Header.Set("Referer", ref.Scheme+"://"+ref.Host+"/")
	}
	response, err := s.webClient.Do(req)
	if err != nil {
		return nil, "", "", userError("网页或图片访问失败，请检查地址、网络或网站访问限制")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, "", "", userError(fmt.Sprintf("网页或图片返回 HTTP %d", response.StatusCode))
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, "", "", userError("读取网页或图片失败")
	}
	if int64(len(b)) > limit {
		return nil, "", "", userError("网页或图片体积超过限制")
	}
	return b, response.Header.Get("Content-Type"), response.Request.URL.String(), nil
}

type webPage struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

func searchBase(c Config) (string, error) {
	if modelSearchEnabled(c) {
		if c.Provider == "ollama" {
			return "", userError("Ollama 原生接口不支持此联网工具，请选择支持 Responses 的接口或独立搜索服务")
		}
		if c.BaseURL == "" || c.Model == "" {
			return "", userError("请先填写 AI 接口地址和模型，或直接提供来源链接")
		}
		return c.BaseURL, nil
	}
	if c.SearchProvider != "tavily" && c.SearchProvider != "searxng" {
		return "", userError("请先配置联网搜索，或直接填写原课程 / 视频链接")
	}
	base := c.SearchURL
	if base == "" && c.SearchProvider == "tavily" {
		base = "https://api.tavily.com"
	}
	if base == "" {
		return "", userError("请填写搜索服务基础地址")
	}
	if c.SearchProvider == "tavily" && c.SearchKey == "" {
		return "", userError("请填写 Tavily 搜索密钥")
	}
	return base, nil
}
func (s *Store) searchPages(ctx context.Context, query string) ([]webPage, error) {
	c := s.getConfig()
	if modelSearchEnabled(c) {
		return s.modelSearchPages(ctx, c, query)
	}
	base, err := searchBase(c)
	if err != nil {
		return nil, err
	}
	var req *http.Request
	if c.SearchProvider == "tavily" {
		payload := map[string]any{"query": query, "search_depth": "basic", "max_results": 5, "include_answer": false, "include_raw_content": false, "include_images": false}
		req, err = http.NewRequestWithContext(ctx, "POST", base+"/search", bytes.NewBufferString(encode(payload)))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+c.SearchKey)
		}
	} else {
		values := url.Values{"q": {query}, "format": {"json"}, "categories": {"general"}}
		req, err = http.NewRequestWithContext(ctx, "GET", base+"/search?"+values.Encode(), nil)
	}
	if err != nil {
		return nil, userError("搜索接口地址无效")
	}
	req.Header.Set("Accept", "application/json")
	status, b, err := s.requestBytes(req, c.RetryCount)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		return nil, userError("搜索接口连接或读取失败，请检查网络或稍后重试")
	}
	if status != 200 {
		return nil, userError(fmt.Sprintf("搜索接口返回 HTTP %d；请检查服务限流、额度、接口配置，或稍后重试", status))
	}
	var result struct {
		Results []webPage `json:"results"`
	}
	if json.Unmarshal(b, &result) != nil {
		return nil, userError("搜索接口没有返回有效 JSON 结果")
	}
	pages := []webPage{}
	seen := map[string]bool{}
	for _, p := range result.Results {
		u, err := publicURL(p.URL)
		if err != nil || seen[u.String()] {
			continue
		}
		p.URL = u.String()
		seen[p.URL] = true
		p.Title = cleanText(p.Title, 300)
		p.Content = cleanText(p.Content, 1500)
		pages = append(pages, p)
		if len(pages) == 5 {
			break
		}
	}
	return pages, nil
}

type pageImages struct {
	Title, Description string
	Images             []string
}

func extractPageImages(raw []byte, base string) (pageImages, error) {
	result := pageImages{Images: []string{}}
	doc, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return result, err
	}
	baseURL, err := publicURL(base)
	if err != nil {
		return result, err
	}
	seen := map[string]bool{}
	add := func(raw string) {
		if len(result.Images) >= 4 {
			return
		}
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			return
		}
		u = baseURL.ResolveReference(u)
		if _, err = publicURL(u.String()); err != nil || seen[u.String()] {
			return
		}
		seen[u.String()] = true
		result.Images = append(result.Images, u.String())
	}
	var jsonImages func(any, int)
	jsonImages = func(value any, depth int) {
		if depth > 12 {
			return
		}
		switch v := value.(type) {
		case []any:
			for _, item := range v {
				jsonImages(item, depth+1)
			}
		case map[string]any:
			for _, key := range []string{"thumbnailUrl", "image"} {
				switch image := v[key].(type) {
				case string:
					add(image)
				case map[string]any:
					if u, ok := image["url"].(string); ok {
						add(u)
					}
				case []any:
					for _, item := range image {
						if u, ok := item.(string); ok {
							add(u)
						} else if obj, ok := item.(map[string]any); ok {
							if u, ok := obj["url"].(string); ok {
								add(u)
							}
						}
					}
				}
			}
			if graph, ok := v["@graph"]; ok {
				jsonImages(graph, depth+1)
			}
		}
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			attrs := map[string]string{}
			for _, attr := range n.Attr {
				attrs[strings.ToLower(attr.Key)] = attr.Val
			}
			switch n.Data {
			case "meta":
				key := strings.ToLower(attrs["property"])
				if key == "" {
					key = strings.ToLower(attrs["name"])
				}
				switch key {
				case "og:image", "og:image:url", "og:image:secure_url", "twitter:image", "twitter:image:src":
					add(attrs["content"])
				case "og:title":
					result.Title = cleanText(attrs["content"], 300)
				case "og:description", "description":
					if result.Description == "" {
						result.Description = cleanText(attrs["content"], 1500)
					}
				}
			case "title":
				if result.Title == "" && n.FirstChild != nil {
					result.Title = cleanText(n.FirstChild.Data, 300)
				}
			case "script":
				if strings.EqualFold(attrs["type"], "application/ld+json") && n.FirstChild != nil {
					var value any
					if json.Unmarshal([]byte(n.FirstChild.Data), &value) == nil {
						jsonImages(value, 0)
					}
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return result, nil
}
