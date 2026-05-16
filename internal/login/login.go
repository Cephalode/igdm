// Package login handles Instagram web authentication (username+password → cookies).
package login

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"go.mau.fi/mautrix-meta/pkg/messagix/cookies"
	"go.mau.fi/mautrix-meta/pkg/messagix/types"
)

const (
	baseURL     = "https://www.instagram.com"
	loginURL    = baseURL + "/api/v1/web/accounts/login/ajax/"
	userAgent   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	igAppID     = "936619743392459"
)

// LoginResult holds the cookies obtained from a successful login.
type LoginResult struct {
	Cookies *cookies.Cookies
	UserID  int64
}

// Login authenticates with Instagram using username+password and returns session cookies.
func Login(ctx context.Context, username, password string) (*LoginResult, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create cookie jar: %w", err)
	}

	client := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
	}

	// Step 1: GET instagram.com to obtain initial cookies (csrftoken, mid)
	log.Info().Str("username", username).Msg("fetching initial page for cookies")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch instagram homepage: %w", err)
	}
	resp.Body.Close()

	// Extract csrftoken and mid from cookies
	var csrfToken, mid, igDid string
	for _, c := range jar.Cookies(&url.URL{Scheme: "https", Host: "www.instagram.com"}) {
		switch c.Name {
		case "csrftoken":
			csrfToken = c.Value
		case "mid":
			mid = c.Value
		case "ig_did":
			igDid = c.Value
		}
	}

	if csrfToken == "" {
		return nil, fmt.Errorf("failed to obtain csrftoken from instagram.com")
	}

	// Step 2: POST login request
	log.Info().Str("username", username).Msg("attempting login")
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	encPassword := fmt.Sprintf("#PWD_INSTAGRAM_BROWSER:0:%s:%s", ts, password)

	formData := url.Values{}
	formData.Set("username", username)
	formData.Set("enc_password", encPassword)
	formData.Set("queryParams", "{}")
	formData.Set("optIntoOneTap", "false")
	formData.Set("stopDeletion", "false")
	formData.Set("trustedDevice", "false")

	req, err = http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRFToken", csrfToken)
	req.Header.Set("X-Instagram-AJAX", "1")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Referer", baseURL+"/")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", baseURL)

	resp, err = client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read login response: %w", err)
	}

	var loginResp struct {
		Authenticated  bool   `json:"authenticated"`
		User           bool   `json:"user"`
		UserID         string `json:"userId"`
		Status         string `json:"status"`
		Message        string `json:"message"`
		TwoFactor      bool   `json:"twoFactorRequired"`
		TwoFactorInfo  struct {
			TwoFactorIdentifier string `json:"two_factor_identifier"`
			Provider            string `json:"provider"`
		} `json:"two_factor_info"`
	}

	if err := json.Unmarshal(body, &loginResp); err != nil {
		return nil, fmt.Errorf("parse login response: %w (body: %s)", err, string(body))
	}

	if loginResp.TwoFactor {
		return nil, fmt.Errorf("two-factor authentication is required — not supported yet (identifier: %s)", loginResp.TwoFactorInfo.TwoFactorIdentifier)
	}

	if !loginResp.Authenticated {
		msg := loginResp.Message
		if msg == "" {
			msg = "unknown error"
		}
		return nil, fmt.Errorf("login failed: %s (status: %s)", msg, loginResp.Status)
	}

	// Step 3: Extract all cookies from the jar
	igURL := &url.URL{Scheme: "https", Host: "www.instagram.com"}
	allCookies := jar.Cookies(igURL)

	cookieMap := make(map[cookies.MetaCookieName]string)
	var dsUserID string
	for _, c := range allCookies {
		switch c.Name {
		case "sessionid":
			cookieMap[cookies.IGCookieSessionID] = c.Value
		case "csrftoken":
			cookieMap[cookies.IGCookieCSRFToken] = c.Value
		case "ds_user_id":
			dsUserID = c.Value
			cookieMap[cookies.IGCookieDSUserID] = c.Value
		case "mid":
			mid = c.Value
			cookieMap[cookies.IGCookieMachineID] = c.Value
		case "ig_did":
			igDid = c.Value
			cookieMap[cookies.IGCookieDeviceID] = c.Value
		}
	}

	// Fallback: use values from initial fetch if not in response cookies
	if _, ok := cookieMap[cookies.IGCookieMachineID]; !ok && mid != "" {
		cookieMap[cookies.IGCookieMachineID] = mid
	}
	if _, ok := cookieMap[cookies.IGCookieDeviceID]; !ok && igDid != "" {
		cookieMap[cookies.IGCookieDeviceID] = igDid
	}

	missing := []string{}
	for _, name := range []cookies.MetaCookieName{
		cookies.IGCookieSessionID, cookies.IGCookieCSRFToken,
		cookies.IGCookieDSUserID, cookies.IGCookieMachineID, cookies.IGCookieDeviceID,
	} {
		if _, ok := cookieMap[name]; !ok {
			missing = append(missing, string(name))
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("login succeeded but missing cookies: %v", missing)
	}

	cks := &cookies.Cookies{Platform: types.Instagram}
	cks.UpdateValues(cookieMap)

	userID, _ := strconv.ParseInt(loginResp.UserID, 10, 64)
	if userID == 0 {
		userID, _ = strconv.ParseInt(dsUserID, 10, 64)
	}

	log.Info().Str("username", username).Int64("user_id", userID).Msg("login successful")
	return &LoginResult{Cookies: cks, UserID: userID}, nil
}
