package oauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
)

type AlipayProvider struct{}

func (p *AlipayProvider) GetName() string {
	return "Alipay"
}

func (p *AlipayProvider) GetProviderPrefix() string {
	return "alipay_"
}

func (p *AlipayProvider) IsEnabled() bool {
	settings := system_setting.GetAlipaySettings()
	return settings != nil && settings.Enabled
}

func (p *AlipayProvider) ExchangeToken(ctx context.Context, code string, c *gin.Context) (*OAuthToken, error) {
	if code == "" {
		return nil, errors.New("missing Alipay auth code")
	}
	settings := system_setting.GetAlipaySettings()
	if err := validateAlipaySettings(settings); err != nil {
		return nil, err
	}

	values, err := buildSignedAlipayValues(settings, map[string]string{
		"method":     "alipay.system.oauth.token",
		"grant_type": "authorization_code",
		"code":       code,
	})
	if err != nil {
		return nil, err
	}

	var envelope alipayTokenEnvelope
	if err := postAlipayForm(ctx, settings.Gateway, values, &envelope); err != nil {
		return nil, err
	}
	if envelope.ErrorResponse.Code != "" {
		return nil, errors.New(envelope.ErrorResponse.SubMsgOrMsg())
	}
	if envelope.Response.AccessToken == "" {
		return nil, errors.New("Alipay token response is empty")
	}

	expiresIn, _ := strconv.Atoi(envelope.Response.ExpiresIn)
	return &OAuthToken{
		AccessToken:  envelope.Response.AccessToken,
		RefreshToken: envelope.Response.RefreshToken,
		ExpiresIn:    expiresIn,
		Scope:        settings.Scope,
		TokenType:    "Bearer",
	}, nil
}

func (p *AlipayProvider) GetUserInfo(ctx context.Context, token *OAuthToken) (*OAuthUser, error) {
	if token == nil || token.AccessToken == "" {
		return nil, errors.New("missing Alipay access token")
	}
	settings := system_setting.GetAlipaySettings()
	if err := validateAlipaySettings(settings); err != nil {
		return nil, err
	}

	values, err := buildSignedAlipayValues(settings, map[string]string{
		"method":     "alipay.user.info.share",
		"auth_token": token.AccessToken,
	})
	if err != nil {
		return nil, err
	}

	var envelope alipayUserInfoEnvelope
	if err := postAlipayForm(ctx, settings.Gateway, values, &envelope); err != nil {
		return nil, err
	}
	if envelope.ErrorResponse.Code != "" {
		return nil, errors.New(envelope.ErrorResponse.SubMsgOrMsg())
	}
	if envelope.Response.UserID == "" {
		return nil, errors.New("Alipay user info response is empty")
	}

	displayName := envelope.Response.NickName
	if displayName == "" {
		displayName = "Alipay User"
	}
	return &OAuthUser{
		ProviderUserID: envelope.Response.UserID,
		Username:       "alipay_" + envelope.Response.UserID,
		DisplayName:    displayName,
		Extra: map[string]any{
			"avatar":   envelope.Response.Avatar,
			"province": envelope.Response.Province,
			"city":     envelope.Response.City,
		},
	}, nil
}

func (p *AlipayProvider) IsUserIDTaken(providerUserID string) bool {
	return model.IsAlipayIdAlreadyTaken(providerUserID)
}

func (p *AlipayProvider) FillUserByProviderID(user *model.User, providerUserID string) error {
	user.AlipayId = providerUserID
	return user.FillUserByAlipayId()
}

func (p *AlipayProvider) SetProviderUserID(user *model.User, providerUserID string) {
	user.AlipayId = providerUserID
}

func validateAlipaySettings(settings *system_setting.AlipaySettings) error {
	if settings == nil || !settings.Enabled {
		return errors.New("Alipay login is disabled")
	}
	if settings.AppId == "" || settings.AppPrivateKey == "" || settings.Gateway == "" {
		return errors.New("Alipay settings are incomplete")
	}
	return nil
}

func buildSignedAlipayValues(settings *system_setting.AlipaySettings, params map[string]string) (url.Values, error) {
	values := url.Values{}
	values.Set("app_id", settings.AppId)
	values.Set("charset", "utf-8")
	values.Set("sign_type", "RSA2")
	values.Set("timestamp", time.Now().Format("2006-01-02 15:04:05"))
	values.Set("version", "1.0")
	for key, value := range params {
		values.Set(key, value)
	}

	sign, err := signAlipayValues(values, settings.AppPrivateKey)
	if err != nil {
		return nil, err
	}
	values.Set("sign", sign)
	return values, nil
}

func signAlipayValues(values url.Values, privateKey string) (string, error) {
	key, err := parseRSAPrivateKey(privateKey)
	if err != nil {
		return "", err
	}
	signContent := alipaySignContent(values)
	sum := sha256.Sum256([]byte(signContent))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(signature), nil
}

func alipaySignContent(values url.Values) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		if key == "sign" || values.Get(key) == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+values.Get(key))
	}
	return strings.Join(pairs, "&")
}

func parseRSAPrivateKey(privateKey string) (*rsa.PrivateKey, error) {
	normalized := strings.TrimSpace(privateKey)
	if !strings.Contains(normalized, "BEGIN") {
		normalized = "-----BEGIN PRIVATE KEY-----\n" + normalized + "\n-----END PRIVATE KEY-----"
	}
	block, _ := pem.Decode([]byte(normalized))
	if block == nil {
		return nil, errors.New("failed to decode Alipay private key")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, errors.New("failed to parse Alipay RSA private key")
}

func postAlipayForm(ctx context.Context, gateway string, values url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("Alipay request failed with status %d: %s", resp.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return err
	}
	return nil
}

type alipayErrorResponse struct {
	Code    string `json:"code"`
	Msg     string `json:"msg"`
	SubCode string `json:"sub_code"`
	SubMsg  string `json:"sub_msg"`
}

func (r alipayErrorResponse) SubMsgOrMsg() string {
	if r.SubMsg != "" {
		return r.SubMsg
	}
	if r.Msg != "" {
		return r.Msg
	}
	return "Alipay request failed"
}

type alipayTokenEnvelope struct {
	Response struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    string `json:"expires_in"`
		UserID       string `json:"user_id"`
	} `json:"alipay_system_oauth_token_response"`
	ErrorResponse alipayErrorResponse `json:"error_response"`
	Sign          string              `json:"sign"`
}

type alipayUserInfoEnvelope struct {
	Response struct {
		UserID   string `json:"user_id"`
		NickName string `json:"nick_name"`
		Avatar   string `json:"avatar"`
		Province string `json:"province"`
		City     string `json:"city"`
	} `json:"alipay_user_info_share_response"`
	ErrorResponse alipayErrorResponse `json:"error_response"`
	Sign          string              `json:"sign"`
}

// ProviderUserIDColumn returns the users-table column storing this provider's user ID.
func (p *AlipayProvider) ProviderUserIDColumn() string {
	return "alipay_id"
}

func init() {
	Register("alipay", &AlipayProvider{})
}
