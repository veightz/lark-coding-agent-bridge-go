// Package onboard implements the first-run QR registration wizard: it
// speaks the PersonalAgent device-registration protocol used by the
// official Lark SDKs (POST /oauth/v1/app/registration, begin + poll),
// renders the QR in the terminal, and returns fresh app credentials.
package onboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mdp/qrterminal/v3"
)

const (
	defaultFeishuDomain = "accounts.feishu.cn"
	defaultLarkDomain   = "accounts.larksuite.com"
	registrationPath    = "/oauth/v1/app/registration"
	source              = "go-sdk/lark-coding-agent-bridge-go"
)

// Result carries the freshly created app's credentials.
type Result struct {
	ClientID     string
	ClientSecret string
	TenantBrand  string // "feishu" or "lark"
	OpenID       string // the operator who scanned the QR
}

type registrationResponse struct {
	VerificationURIComplete string `json:"verification_uri_complete"`
	DeviceCode              string `json:"device_code"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`

	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	UserInfo     *struct {
		OpenID      string `json:"open_id"`
		TenantBrand string `json:"tenant_brand"`
	} `json:"user_info"`

	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// RunWizard drives the terminal QR flow and blocks until the user finishes
// (or the code expires). Ported from the node-sdk's registerApp().
func RunWizard(ctx context.Context) (*Result, error) {
	fmt.Println("\n未检测到飞书应用配置，进入扫码创建向导。")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	baseURL := "https://" + defaultFeishuDomain
	larkBaseURL := "https://" + defaultLarkDomain

	begin, err := requestRegistration(ctx, httpClient, baseURL, url.Values{
		"action":            {"begin"},
		"archetype":         {"PersonalAgent"},
		"auth_method":       {"client_secret"},
		"request_user_info": {"open_id"},
	})
	if err != nil {
		return nil, fmt.Errorf("初始化扫码注册失败: %w", err)
	}
	if begin.VerificationURIComplete == "" || begin.DeviceCode == "" {
		return nil, fmt.Errorf("注册服务返回异常：缺少 verification_uri_complete 或 device_code")
	}

	qrURL, err := url.Parse(begin.VerificationURIComplete)
	if err != nil {
		return nil, err
	}
	q := qrURL.Query()
	q.Set("from", "sdk")
	q.Set("source", source)
	q.Set("tp", "sdk")
	qrURL.RawQuery = q.Encode()

	expireIn := begin.ExpiresIn
	if expireIn <= 0 {
		expireIn = 600
	}
	interval := begin.Interval
	if interval <= 0 {
		interval = 5
	}

	fmt.Println("\n请用飞书 App 扫描以下二维码完成应用创建：")
	qrterminal.GenerateHalfBlock(qrURL.String(), qrterminal.M, os.Stdout)
	fmt.Printf("\n二维码有效期：约 %d 分钟\n", (expireIn+30)/60)
	fmt.Printf("也可以直接在浏览器打开：%s\n\n", qrURL.String())

	deadline := time.Now().Add(time.Duration(expireIn) * time.Second)
	domainSwitched := false
	pollInterval := time.Duration(interval) * time.Second

	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("二维码已过期，请重新运行")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}

		poll, err := requestRegistration(ctx, httpClient, baseURL, url.Values{
			"action":      {"poll"},
			"device_code": {begin.DeviceCode},
		})
		if err != nil {
			return nil, fmt.Errorf("轮询注册状态失败: %w", err)
		}

		// International tenant: switch to the Lark domain once.
		if poll.UserInfo != nil && poll.UserInfo.TenantBrand == "lark" && !domainSwitched {
			baseURL = larkBaseURL
			domainSwitched = true
			fmt.Println("识别到国际版租户，已切换到 larksuite.com 域名。")
			continue
		}

		if poll.ClientID != "" && poll.ClientSecret != "" {
			tenant := "feishu"
			openID := ""
			if poll.UserInfo != nil {
				if poll.UserInfo.TenantBrand != "" {
					tenant = poll.UserInfo.TenantBrand
				}
				openID = poll.UserInfo.OpenID
			}
			fmt.Println("\n✓ 应用创建成功")
			fmt.Printf("  App ID:  %s\n", poll.ClientID)
			fmt.Printf("  Tenant:  %s\n", tenant)
			if openID != "" {
				fmt.Printf("  Creator: %s\n", openID)
			}
			fmt.Println()
			return &Result{
				ClientID:     poll.ClientID,
				ClientSecret: poll.ClientSecret,
				TenantBrand:  tenant,
				OpenID:       openID,
			}, nil
		}

		switch poll.Error {
		case "", "authorization_pending":
			// keep polling
		case "slow_down":
			pollInterval += 5 * time.Second
			fmt.Println("轮询速度过快，已自动降速。")
		case "access_denied", "expired_token":
			desc := poll.ErrorDescription
			if desc == "" {
				desc = poll.Error
			}
			return nil, fmt.Errorf("扫码注册失败: %s", desc)
		default:
			desc := poll.ErrorDescription
			if desc == "" {
				desc = poll.Error
			}
			return nil, fmt.Errorf("扫码注册失败: %s", desc)
		}
	}
}

func requestRegistration(ctx context.Context, client *http.Client, baseURL string, form url.Values) (*registrationResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+registrationPath,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// RFC 8628: authorization_pending / slow_down come back as HTTP 400 with
	// a JSON body, so decode regardless of status.
	var out registrationResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("注册服务返回非 JSON (HTTP %d): %s", resp.StatusCode, truncate(string(body), 200))
	}
	return &out, nil
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
