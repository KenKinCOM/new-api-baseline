package middleware

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

// 客户端幂等键（G-07）的 HTTP 层。
//
// 语义（与支付网关一致）：
//
//	没有 Idempotency-Key 头      → 完全走原路径，一个字节都不影响；
//	首次见到这个键              → 正常执行（会扣费），并把首次响应回填到键上；
//	再次见到同键 + 同内容        → 回放首次响应，不执行、不重复扣费；
//	再次见到同键 + 内容不同      → 409，明确说明键被复用在别的请求上，
//	                             绝不返回第一次的结果（那会是别的请求的答案）。
//
// 失败语义：5xx / panic 会释放键，让客户端能用同一个键重试；4xx 视为确定性
// 结果，按首次响应回放（同一个坏请求重发一次也不会变成成功）。

const (
	// IdempotencyKeyHeader 是客户端携带幂等键的请求头（IETF draft 的字段名）。
	IdempotencyKeyHeader = "Idempotency-Key"
	// idempotencyReplayedHeader 告诉客户端「这次是回放，不是真的又执行了一次」。
	idempotencyReplayedHeader = "Idempotency-Replayed"
	// idempotencyRequestIdHeader 把首次请求的 request id 带给重复请求，便于对账。
	idempotencyFirstRequestIdHeader = "Idempotency-First-Request-Id"
)

// ClientIdempotencyKey 读取 Idempotency-Key 并在有键时施加幂等语义。
//
// 必须放在 TokenAuth 之后：幂等域是（用户 + 令牌 + 键），没有鉴权上下文就无法
// 区分「两个客户端恰好用了同一个 UUID」。未鉴权路由会原样放行。
func ClientIdempotencyKey() gin.HandlerFunc {
	return func(c *gin.Context) {
		setting := operation_setting.GetIdempotencySetting()
		if setting == nil || !setting.Enabled {
			c.Next()
			return
		}

		key := strings.TrimSpace(c.GetHeader(IdempotencyKeyHeader))
		if key == "" {
			c.Next()
			return
		}
		if len(key) > model.IdempotencyKeyMaxLength {
			abortIdempotencyError(c, http.StatusBadRequest, "invalid_idempotency_key",
				fmt.Sprintf("%s must not exceed %d characters", IdempotencyKeyHeader, model.IdempotencyKeyMaxLength))
			return
		}
		// 只对会改变状态的请求做幂等：GET/HEAD 没有 body、也不产生费用；
		// WebSocket 升级是一次长连接，不能按「请求-响应」回放。
		if !idempotentMethod(c.Request.Method) || isWebSocketUpgrade(c) {
			c.Next()
			return
		}

		userId := c.GetInt("id")
		tokenId := c.GetInt("token_id")
		if userId <= 0 || tokenId <= 0 {
			c.Next()
			return
		}

		body, err := common.GetBodyStorage(c)
		if err != nil {
			if common.IsRequestBodyTooLargeError(err) {
				abortIdempotencyError(c, http.StatusRequestEntityTooLarge, "request_body_too_large", err.Error())
				return
			}
			abortIdempotencyError(c, http.StatusBadRequest, "invalid_request_body", err.Error())
			return
		}
		digest, err := service.IdempotencyRequestDigest(c.Request.Method, idempotencyPath(c), body)
		if err != nil {
			abortIdempotencyError(c, http.StatusInternalServerError, "idempotency_digest_failed",
				"failed to read the request body for idempotency")
			return
		}

		scope := service.IdempotencyScope{
			UserId:    userId,
			TokenId:   tokenId,
			Key:       key,
			Route:     c.Request.Method + " " + idempotencyPath(c),
			Digest:    digest,
			RequestId: c.GetString(common.RequestIdKey),
		}
		outcome, err := service.BeginIdempotentRequest(scope)
		if err != nil {
			var conflict *service.IdempotencyConflictError
			if errors.As(err, &conflict) {
				abortIdempotencyConflict(c, conflict)
				return
			}
			// 查不动库时宁可拒绝也不能放行：放行就是一次可能的重复扣费。
			common.SysError(common.LogText("idempotency begin failed (key=%s): %s", key, err.Error()))
			abortIdempotencyError(c, http.StatusServiceUnavailable, "idempotency_unavailable",
				"idempotency key could not be reserved, please retry later")
			return
		}

		if outcome.Replay != nil {
			writeIdempotencyReplay(c, outcome.Replay)
			return
		}

		// 本次请求持有这个键：捕获首次响应，结束后按状态码回填或释放。
		recorder := newIdempotencyRecorder(c.Writer, setting.EffectiveMaxReplayBytes())
		c.Writer = recorder
		defer func() {
			// 还原 writer，避免外层中间件看到包装类型。
			c.Writer = recorder.ResponseWriter
		}()
		defer func() {
			if r := recover(); r != nil {
				// panic 由外层的 Recovery 换成 500：这里先释放键，
				// 让客户端拿同一个键重试，而不是被一次崩溃锁住。
				service.AbortIdempotentRequest(outcome.Record, "handler panicked")
				panic(r)
			}
		}()

		c.Next()
		settleIdempotentRequest(outcome.Record, recorder)
	}
}

func idempotentMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func isWebSocketUpgrade(c *gin.Context) bool {
	return strings.EqualFold(c.GetHeader("Upgrade"), "websocket")
}

// idempotencyPath 用真实请求路径（而不是路由模板）：同一个键被用到另一个
// 接口上时，摘要必须不同，才会被判成「键复用」而不是回放。
func idempotencyPath(c *gin.Context) string {
	if c.Request == nil || c.Request.URL == nil {
		return ""
	}
	return c.Request.URL.Path
}

// settleIdempotentRequest 把首次响应登记到键上。
//
// 5xx / 没写出任何响应视为「这次没成」：释放键，允许客户端重试。
// 其余状态码（含 4xx）都是确定性结果，回填后重复请求回放同样的响应。
func settleIdempotentRequest(record *model.IdempotencyKey, recorder *idempotencyRecorder) {
	if record == nil || recorder == nil {
		return
	}
	status := recorder.Status()
	if status <= 0 || status >= http.StatusInternalServerError {
		service.AbortIdempotentRequest(record, fmt.Sprintf("request failed with status %d", status))
		return
	}
	contentType := recorder.Header().Get("Content-Type")
	var body []byte
	if !recorder.overflow {
		body = recorder.captured()
	}
	// 超出回放上限（或流式响应超限）时 replayable=false：重复请求会收到 409，
	// 而不是一个被截断的 body。
	service.CompleteIdempotentRequest(record, status, contentType, body, !recorder.overflow)
}

func writeIdempotencyReplay(c *gin.Context, replay *service.IdempotencyReplay) {
	c.Header(idempotencyReplayedHeader, "true")
	if replay.RequestId != "" {
		c.Header(idempotencyFirstRequestIdHeader, replay.RequestId)
	}
	if replay.ContentType != "" {
		c.Header("Content-Type", replay.ContentType)
	}
	c.Status(replay.Status)
	if len(replay.Body) > 0 {
		_, _ = c.Writer.Write(replay.Body)
	}
	c.Abort()
}

func abortIdempotencyConflict(c *gin.Context, conflict *service.IdempotencyConflictError) {
	code := "idempotency_key_reused"
	switch conflict.Reason {
	case service.IdempotencyReasonInProgress:
		code = "idempotency_request_in_progress"
		// 上一笔还在跑：让客户端等一会儿用同一个键重试。
		c.Header("Retry-After", "1")
	case service.IdempotencyReasonNotReplayable:
		code = "idempotency_response_not_replayable"
	}
	if conflict.FirstRequestId != "" {
		c.Header(idempotencyFirstRequestIdHeader, conflict.FirstRequestId)
	}
	c.AbortWithStatusJSON(http.StatusConflict, gin.H{
		"error": gin.H{
			"message": conflict.Error(),
			"type":    "invalid_request_error",
			"code":    code,
		},
	})
}

func abortIdempotencyError(c *gin.Context, status int, code string, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    "invalid_request_error",
			"code":    code,
		},
	})
}

// idempotencyRecorder 边写边截留响应体（不改变对客户端的即时输出）。
//
// 只截留不重放：写超时就丢缓冲标 overflow，绝不在写路径上返回错误。
type idempotencyRecorder struct {
	gin.ResponseWriter
	buf      bytes.Buffer
	maxBytes int
	overflow bool
	status   int
}

func newIdempotencyRecorder(writer gin.ResponseWriter, maxBytes int) *idempotencyRecorder {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	return &idempotencyRecorder{ResponseWriter: writer, maxBytes: maxBytes}
}

func (w *idempotencyRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *idempotencyRecorder) WriteHeaderNow() {
	if w.status == 0 {
		w.status = w.ResponseWriter.Status()
	}
	w.ResponseWriter.WriteHeaderNow()
}

func (w *idempotencyRecorder) Write(data []byte) (int, error) {
	w.capture(data)
	return w.ResponseWriter.Write(data)
}

func (w *idempotencyRecorder) WriteString(data string) (int, error) {
	w.capture([]byte(data))
	return w.ResponseWriter.WriteString(data)
}

func (w *idempotencyRecorder) capture(data []byte) {
	if w.overflow || len(data) == 0 {
		return
	}
	if w.buf.Len()+len(data) > w.maxBytes {
		w.overflow = true
		w.buf.Reset()
		return
	}
	_, _ = w.buf.Write(data)
}

func (w *idempotencyRecorder) captured() []byte {
	if w.buf.Len() == 0 {
		return nil
	}
	out := make([]byte, w.buf.Len())
	copy(out, w.buf.Bytes())
	return out
}

// Status 返回真正写给客户端的状态码。
func (w *idempotencyRecorder) Status() int {
	if w.status != 0 {
		return w.status
	}
	return w.ResponseWriter.Status()
}
