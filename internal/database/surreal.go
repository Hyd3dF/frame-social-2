package database

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

type Client struct {
	database  string
	http      *http.Client
	namespace string
	password  string
	rpcURL    string
	sequence  atomic.Uint64
	username  string
}

type rpcRequest struct {
	ID     uint64 `json:"id"`
	Method string `json:"method"`
	Params []any  `json:"params"`
}

type rpcResponse struct {
	Error  *rpcError       `json:"error"`
	Result json.RawMessage `json:"result"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type statementResult struct {
	Result json.RawMessage `json:"result"`
	Status string          `json:"status"`
}

func New(url, namespace, database, username, password string) *Client {
	transport := &http.Transport{
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		MaxConnsPerHost:       64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
	}
	return &Client{
		database:  database,
		http:      &http.Client{Timeout: 10 * time.Second, Transport: transport},
		namespace: namespace,
		password:  password,
		rpcURL:    strings.TrimRight(url, "/") + "/rpc",
		username:  username,
	}
}

func (c *Client) Query(ctx context.Context, sql string, vars map[string]any, destination any) error {
	payload, err := json.Marshal(rpcRequest{ID: c.sequence.Add(1), Method: "query", Params: []any{sql, vars}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.rpcURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Surreal-NS", c.namespace)
	req.Header.Set("Surreal-DB", c.database)
	req.Header.Set("Surreal-Auth-NS", c.namespace)
	req.Header.Set("Surreal-Auth-DB", c.database)
	req.SetBasicAuth(c.username, c.password)
	response, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("surreal request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("surreal returned HTTP %d", response.StatusCode)
	}
	var envelope rpcResponse
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode surreal response: %w", err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("surreal RPC %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	var statements []statementResult
	if err := json.Unmarshal(envelope.Result, &statements); err != nil {
		return fmt.Errorf("decode surreal statements: %w", err)
	}
	if len(statements) == 0 {
		return errors.New("surreal returned no statements")
	}
	for _, statement := range statements {
		if statement.Status != "OK" {
			return fmt.Errorf("surreal query failed: %s", string(statement.Result))
		}
	}
	if destination == nil {
		return nil
	}
	return json.Unmarshal(statements[len(statements)-1].Result, destination)
}

func (c *Client) Ping(ctx context.Context) error {
	var result map[string]any
	return c.Query(ctx, "RETURN { ok: true };", nil, &result)
}
