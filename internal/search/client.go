// Package search 为天大校园数据与资料提供毫秒级全文检索能力。
// 基于 Go 标准库实现零依赖轻量级 HTTP 客户端。
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client 包含与 MeiliSearch 交互的基础配置与 HTTP 客户端。
type Client struct {
	baseURL string
	apiKey  string
	httpCli *http.Client
}

// NewClient 创建新的 MeiliSearch 检索客户端。
func NewClient(rawURL, apiKey string) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return nil, errors.New("invalid meilisearch url")
	}
	base := fmt.Sprintf("%s://%s%s", parsed.Scheme, parsed.Host, strings.TrimRight(parsed.Path, "/"))
	return &Client{
		baseURL: base,
		apiKey:  apiKey,
		httpCli: &http.Client{
			Timeout: 5 * time.Second,
		},
	}, nil
}

// SearchRequest 定义通用的 MeiliSearch 搜索请求体。
type SearchRequest struct {
	Q                    string   `json:"q"`
	Limit                int      `json:"limit,omitempty"`
	Offset               int      `json:"offset,omitempty"`
	Filter               any      `json:"filter,omitempty"`
	Sort                 []string `json:"sort,omitempty"`
	AttributesToRetrieve []string `json:"attributesToRetrieve,omitempty"`
}

// SearchResponse 定义通用的 MeiliSearch 搜索响应体。
type SearchResponse[T any] struct {
	Hits               []T    `json:"hits"`
	Query              string `json:"query"`
	ProcessingTimeMs   int    `json:"processingTimeMs"`
	Limit              int    `json:"limit"`
	Offset             int    `json:"offset"`
	EstimatedTotalHits int    `json:"estimatedTotalHits"`
}

// TaskResponse 异步索引操作返回的任务基本信息。
type TaskResponse struct {
	TaskUID    int64  `json:"taskUid"`
	IndexUID   string `json:"indexUid"`
	Status     string `json:"status"`
	Type       string `json:"type"`
	EnqueuedAt string `json:"enqueuedAt"`
}

// EnsureIndex 确保指定的索引存在；若不存在则自动创建。
func (c *Client) EnsureIndex(ctx context.Context, indexUID, primaryKey string) error {
	payload := map[string]string{
		"uid": indexUID,
	}
	if primaryKey != "" {
		payload["primaryKey"] = primaryKey
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/indexes", bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.setHeaders(req)

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusAccepted {
		return nil
	}
	if resp.StatusCode == http.StatusBadRequest {
		respBytes, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(respBytes), "already_exists") {
			return nil
		}
		return fmt.Errorf("meilisearch ensure index failed: %s", string(respBytes))
	}

	respBytes, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("meilisearch unexpected status %d: %s", resp.StatusCode, string(respBytes))
}

// AddDocuments 批量将文档推送至指定索引。
func (c *Client) AddDocuments(ctx context.Context, indexUID string, docs any) (*TaskResponse, error) {
	body, err := json.Marshal(docs)
	if err != nil {
		return nil, err
	}

	targetURL := fmt.Sprintf("%s/indexes/%s/documents", c.baseURL, url.PathEscape(indexUID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		respBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("add documents failed (%d): %s", resp.StatusCode, string(respBytes))
	}

	var task TaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		return nil, err
	}
	return &task, nil
}

// Search 执行全文检索。
func Search[T any](ctx context.Context, c *Client, indexUID string, searchReq SearchRequest) (*SearchResponse[T], error) {
	if c == nil {
		return nil, errors.New("search client is uninitialized")
	}
	body, err := json.Marshal(searchReq)
	if err != nil {
		return nil, err
	}

	targetURL := fmt.Sprintf("%s/indexes/%s/search", c.baseURL, url.PathEscape(indexUID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("meilisearch query failed (%d): %s", resp.StatusCode, string(respBytes))
	}

	var result SearchResponse[T]
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}
