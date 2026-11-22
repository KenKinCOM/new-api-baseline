package middleware

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

// envTrustedProxiesRequired turns the historical "warn and trust RFC1918" default
// into a startup failure. Production deployments should set it: silently trusting
// loopback + private ranges means any client that can reach the port from a
// private network can spoof X-Forwarded-For and bypass IP-based rate limits and
// audit attribution (T4 §4.5 #14).
const envTrustedProxiesRequired = "TRUSTED_PROXIES_REQUIRED"

func ConfigureTrustedProxies(engine *gin.Engine) error {
	trustedProxies, usedDefaults, err := common.ResolveTrustedProxies(os.Getenv("TRUSTED_PROXIES"))
	if err != nil {
		return err
	}
	if usedDefaults {
		if trustedProxiesRequired() {
			return fmt.Errorf("%s is true but TRUSTED_PROXIES is unset or blank; set it to the ALB/proxy CIDRs, or to \"none\" for direct exposure", envTrustedProxiesRequired)
		}
		log.Print(common.LogText("WARNING: TRUSTED_PROXIES is unset or blank; trusting loopback, RFC 1918, and IPv6 ULA proxy addresses for compatibility. Set TRUSTED_PROXIES=none to trust no proxies, or configure explicit proxy IPs/CIDRs to replace these defaults."))
	}
	return common.ConfigureTrustedProxies(engine, trustedProxies)
}

func trustedProxiesRequired() bool {
	raw := strings.TrimSpace(os.Getenv(envTrustedProxiesRequired))
	if raw == "" {
		return false
	}
	required, err := strconv.ParseBool(raw)
	if err != nil {
		return false
	}
	return required
}
