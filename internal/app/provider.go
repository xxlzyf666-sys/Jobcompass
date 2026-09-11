package app

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

//go:embed prompt.txt
var diagnosisPrompt string

type Usage struct {
	Input  int `json:"input_tokens"`
	Output int `json:"output_tokens"`
}
type Provider interface {
	Ready() bool
	Diagnose(context.Context, Input) (Report, Usage, error)
}

type ProviderError struct {
	Kind, Message string
	Retryable     bool
}

func (e *ProviderError) Error() string { return e.Kind }

type ClaudeProvider struct {
	config Config
	client *http.Client
}

func NewClaudeProvider(config Config) *ClaudeProvider {
	return &ClaudeProvider{config: config, client: &http.Client{Timeout: config.RequestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}
}
func (p *ClaudeProvider) Ready() bool { return p.config.APIKey != "" && p.config.Model != "" }

func (p *ClaudeProvider) Diagnose(ctx context.Context, input Input) (Report, Usage, error) {
	var report Report
	var usage Usage
	if !p.Ready() {
		return report, usage, &ProviderError{"unavailable", "真实诊断暂未开放，可先查看示例报告。", false}
	}
	material, _ := json.Marshal(input)
	body, err := json.Marshal(map[string]any{
		"model": p.config.Model, "max_tokens": 6000,
		"system":      diagnosisPrompt,
		"messages":    []any{map[string]any{"role": "user", "content": string(material)}},
		"tools":       []any{map[string]any{"name": "deliver_diagnosis", "description": "交付有原文依据的岗位对照及补充问题。", "input_schema": reportSchema()}},
		"tool_choice": map[string]string{"type": "tool", "name": "deliver_diagnosis"},
	})
	if err != nil {
		return report, usage, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.config.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return report, usage, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.config.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return report, usage, ctx.Err()
		}
		return report, usage, &ProviderError{"network", "分析服务暂时未能连接，请稍后重新提交。", true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Upstream bodies can repeat input, credentials, or proxy details. Never surface or log them.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			return report, usage, &ProviderError{"upstream_temporary", "分析服务繁忙，请稍后重新提交。", true}
		}
		return report, usage, &ProviderError{"upstream_rejected", "分析服务暂时无法处理这次请求，请稍后再试。", false}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 768*1024+1))
	if err != nil {
		return report, usage, &ProviderError{"response_read", "分析结果未完整返回，请稍后重试。", true}
	}
	if len(data) > 768*1024 {
		return report, usage, &ProviderError{"response_size", "分析结果超过长度限制，请精简材料后重试。", false}
	}
	var envelope struct {
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Usage      Usage  `json:"usage"`
		Content    []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return report, usage, &ProviderError{"invalid_response", "分析结果格式异常，请稍后重试。", false}
	}
	usage = envelope.Usage
	if envelope.StopReason == "refusal" {
		return report, usage, &ProviderError{"refusal", "模型未能处理这份材料，请移除与求职无关的内容后重试。", false}
	}
	if envelope.StopReason == "max_tokens" {
		return report, usage, &ProviderError{"truncated", "分析结果未完成，请缩短材料后重试。", false}
	}
	var toolInput json.RawMessage
	for _, block := range envelope.Content {
		if block.Type == "tool_use" && block.Name == "deliver_diagnosis" {
			if toolInput != nil {
				return report, usage, &ProviderError{"duplicate_output", "分析结果格式异常，请重新提交。", false}
			}
			toolInput = block.Input
		}
	}
	if toolInput == nil {
		return report, usage, &ProviderError{"missing_output", "模型没有生成完整的对照结果，请重新提交。", false}
	}
	decoder := json.NewDecoder(bytes.NewReader(toolInput))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return report, usage, &ProviderError{"invalid_schema", "分析结果结构不完整，请重新提交。", false}
	}
	if err := validateReport(&report, input); err != nil {
		return report, usage, &ProviderError{"ungrounded", "本次结果有引文或结构未通过核对，已停止展示。请检查材料后重试。", false}
	}
	report.Model = p.config.Model
	if envelope.Model != "" && len(envelope.Model) < 120 {
		report.Model = envelope.Model
	}
	return report, usage, nil
}

func providerFailure(err error) (message, kind string, retryable bool) {
	var p *ProviderError
	if errors.As(err, &p) {
		return p.Message, p.Kind, p.Retryable
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "本次分析超时，请稍后重试。", "timeout", true
	}
	if errors.Is(err, context.Canceled) {
		return "分析已取消。", "cancelled", false
	}
	return "暂时无法完成分析，请稍后重试。", "internal", false
}

func normalizeRecovery(value string) (string, error) {
	v := strings.ToUpper(strings.Join(strings.Fields(value), ""))
	v = strings.ReplaceAll(v, "-", "")
	if len(v) != 42 || !strings.HasPrefix(v, "JC") {
		return "", errors.New("恢复码格式不正确，请粘贴完整的 JC 开头的恢复码。")
	}
	for _, r := range v[2:] {
		if !(r >= '0' && r <= '9') && !(r >= 'A' && r <= 'F') {
			return "", fmt.Errorf("恢复码包含无效字符。")
		}
	}
	return v, nil
}
