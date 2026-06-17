package system_setting

import "github.com/QuantumNous/new-api/setting/config"

type AlipaySettings struct {
	Enabled         bool   `json:"enabled"`
	AppId           string `json:"app_id"`
	AppPrivateKey   string `json:"app_private_key"`
	AlipayPublicKey string `json:"alipay_public_key"`
	Gateway         string `json:"gateway"`
	Scope           string `json:"scope"`
}

var alipaySettings = AlipaySettings{
	Gateway: "https://openapi.alipay.com/gateway.do",
	Scope:   "auth_user",
}

func GetAlipaySettings() *AlipaySettings {
	return &alipaySettings
}

func init() {
	config.GlobalConfig.Register("alipay", &alipaySettings)
}
