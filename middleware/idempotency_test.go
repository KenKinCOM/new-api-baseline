package middleware

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
)

// G-07 客户端幂等键的 HTTP 层测试。
//
// 两个必须覆盖的口径：
//  1. 同键 + 同内容 → 第二次直接拿到第一次的响应，业务处理只执行一次（不重复扣费）；
//  2. 同键 + 不同内容 → 409，且响应里绝不出现第一次的结果。

const idempotencyTestPath = "/v1/chat/completions"

func setupIdempotencyMiddlewareTest(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	previousDB := model.DB
	previousLogDB := model.LOG_DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	// :memory: 每个连接是一个独立库，必须压成单连接。
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.IdempotencyKey{}))

	model.DB = db
	model.LOG_DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
	})
}

// newIdempotencyTestEngine 模拟 TokenAuth 之后的链路：先放用户/令牌上下文，
// 再走幂等中间件，最后是真正会「扣费」的处理器。
func newIdempotencyTestEngine(handler gin.HandlerFunc) *gin.Engine {
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("id", 42)
		c.Set("token_id", 7)
		c.Set(common.RequestIdKey, "req-"+c.GetHeader("X-Test-Request-Id"))
		c.Next()
	})
	engine.Use(ClientIdempotencyKey())
	engine.POST(idempotencyTestPath, handler)
	engine.POST("/v1/embeddings", handler)
	return engine
}

func postIdempotentRequest(t *testing.T, engine *gin.Engine, key string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, idempotencyTestPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(IdempotencyKeyHeader, key)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	return recorder
}

func decodeIdempotencyError(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	errObj, ok := payload["error"].(map[string]any)
	require.True(t, ok, "response must carry an error object: %s", recorder.Body.String())
	return errObj
}

// TestClientIdempotencyKeyReplaysFirstResponse 覆盖「相同内容返回首次结果」。
func TestClientIdempotencyKeyReplaysFirstResponse(t *testing.T) {
	setupIdempotencyMiddlewareTest(t)

	calls := 0
	engine := newIdempotencyTestEngine(func(c *gin.Context) {
		calls++
		c.JSON(http.StatusOK, gin.H{"id": "chatcmpl-first", "calls": calls})
	})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`

	first := postIdempotentRequest(t, engine, "idem-replay-1", body)
	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, 1, calls)
	assert.Empty(t, first.Header().Get(idempotencyReplayedHeader), "首次响应不是回放")

	second := postIdempotentRequest(t, engine, "idem-replay-1", body)
	assert.Equal(t, http.StatusOK, second.Code)
	assert.Equal(t, "true", second.Header().Get(idempotencyReplayedHeader))
	assert.Equal(t, first.Body.String(), second.Body.String(), "回放必须逐字节等于首次响应")
	assert.Equal(t, 1, calls, "回放不能再执行业务逻辑（不重复扣费）")
	assert.Equal(t, "application/json; charset=utf-8", second.Header().Get("Content-Type"))
}

// TestClientIdempotencyKeyRejectsDifferentPayload 覆盖「不同内容返回 409」。
func TestClientIdempotencyKeyRejectsDifferentPayload(t *testing.T) {
	setupIdempotencyMiddlewareTest(t)

	calls := 0
	engine := newIdempotencyTestEngine(func(c *gin.Context) {
		calls++
		c.JSON(http.StatusOK, gin.H{"id": "chatcmpl-first"})
	})

	first := postIdempotentRequest(t, engine, "idem-reuse-1", `{"model":"gpt-4o","messages":[{"role":"user","content":"A"}]}`)
	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, 1, calls)

	second := postIdempotentRequest(t, engine, "idem-reuse-1", `{"model":"gpt-4o","messages":[{"role":"user","content":"B"}]}`)
	require.Equal(t, http.StatusConflict, second.Code)
	assert.Equal(t, 1, calls, "内容不同时必须拒绝第二次，不能执行")

	errObj := decodeIdempotencyError(t, second)
	assert.Equal(t, "idempotency_key_reused", errObj["code"])
	assert.Contains(t, errObj["message"], "different request payload")
	// 关键口径：不能把第一次的结果当成第二次的答案返回。
	assert.NotContains(t, second.Body.String(), "chatcmpl-first")
	assert.Empty(t, second.Header().Get(idempotencyReplayedHeader))
}

// TestClientIdempotencyKeyScopesDigestByPath 同一个键被用到另一个接口同样算复用。
func TestClientIdempotencyKeyScopesDigestByPath(t *testing.T) {
	setupIdempotencyMiddlewareTest(t)

	calls := 0
	engine := newIdempotencyTestEngine(func(c *gin.Context) {
		calls++
		c.JSON(http.StatusOK, gin.H{"path": c.Request.URL.Path})
	})

	first := postIdempotentRequest(t, engine, "idem-cross-path", `{"input":"x"}`)
	require.Equal(t, http.StatusOK, first.Code)

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"input":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(IdempotencyKeyHeader, "idem-cross-path")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusConflict, recorder.Code)
	assert.Equal(t, 1, calls)
}

// TestClientIdempotencyKeyWithoutHeaderPassesThrough 没带头的请求一个字节都不影响。
func TestClientIdempotencyKeyWithoutHeaderPassesThrough(t *testing.T) {
	setupIdempotencyMiddlewareTest(t)

	calls := 0
	engine := newIdempotencyTestEngine(func(c *gin.Context) {
		calls++
		c.JSON(http.StatusOK, gin.H{"calls": calls})
	})

	body := `{"model":"gpt-4o","messages":[]}`
	require.Equal(t, http.StatusOK, postIdempotentRequest(t, engine, "", body).Code)
	require.Equal(t, http.StatusOK, postIdempotentRequest(t, engine, "", body).Code)
	assert.Equal(t, 2, calls)

	count, err := model.CountIdempotencyKeys()
	require.NoError(t, err)
	assert.Equal(t, int64(0), count, "没带 Idempotency-Key 的请求不应该写表")
}

// TestClientIdempotencyKeyReleasedOnServerError 5xx 释放键，客户端可用同键重试。
func TestClientIdempotencyKeyReleasedOnServerError(t *testing.T) {
	setupIdempotencyMiddlewareTest(t)

	calls := 0
	engine := newIdempotencyTestEngine(func(c *gin.Context) {
		calls++
		if calls == 1 {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "upstream failed"}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": "chatcmpl-retry-ok"})
	})

	body := `{"model":"gpt-4o","messages":[]}`
	first := postIdempotentRequest(t, engine, "idem-retry-1", body)
	require.Equal(t, http.StatusBadGateway, first.Code)

	second := postIdempotentRequest(t, engine, "idem-retry-1", body)
	assert.Equal(t, http.StatusOK, second.Code, "服务端失败后同键重试必须重新执行")
	assert.Equal(t, 2, calls)
	assert.Contains(t, second.Body.String(), "chatcmpl-retry-ok")
}

// TestClientIdempotencyKeyReplaysClientError 4xx 是确定性结果，按首次响应回放。
func TestClientIdempotencyKeyReplaysClientError(t *testing.T) {
	setupIdempotencyMiddlewareTest(t)

	calls := 0
	engine := newIdempotencyTestEngine(func(c *gin.Context) {
		calls++
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "model not allowed"}})
	})

	body := `{"model":"gpt-4o","messages":[]}`
	first := postIdempotentRequest(t, engine, "idem-4xx", body)
	require.Equal(t, http.StatusBadRequest, first.Code)

	second := postIdempotentRequest(t, engine, "idem-4xx", body)
	assert.Equal(t, http.StatusBadRequest, second.Code)
	assert.Equal(t, "true", second.Header().Get(idempotencyReplayedHeader))
	assert.Equal(t, first.Body.String(), second.Body.String())
	assert.Equal(t, 1, calls)
}

// TestClientIdempotencyKeyInProgressConflict 第一次还在跑时同键请求 409，不并发执行。
func TestClientIdempotencyKeyInProgressConflict(t *testing.T) {
	setupIdempotencyMiddlewareTest(t)

	body := `{"model":"gpt-4o","messages":[]}`
	storage, err := common.CreateBodyStorage([]byte(body))
	require.NoError(t, err)
	t.Cleanup(func() { _ = storage.Close() })
	digest, err := service.IdempotencyRequestDigest(http.MethodPost, idempotencyTestPath, storage)
	require.NoError(t, err)

	// 模拟「第一个请求正在执行」：库里留着 pending 行，租约还没过。
	_, owned, err := model.CreatePendingIdempotencyKey(&model.IdempotencyKey{
		UserId:        42,
		TokenId:       7,
		Key:           "idem-inflight",
		Route:         http.MethodPost + " " + idempotencyTestPath,
		RequestDigest: digest,
		Status:        model.IdempotencyStatusPending,
		RequestId:     "req-inflight",
		ExpiresAt:     common.GetTimestamp() + 60,
	})
	require.NoError(t, err)
	require.True(t, owned)

	calls := 0
	engine := newIdempotencyTestEngine(func(c *gin.Context) {
		calls++
		c.JSON(http.StatusOK, gin.H{"id": "should-not-run"})
	})

	recorder := postIdempotentRequest(t, engine, "idem-inflight", body)
	require.Equal(t, http.StatusConflict, recorder.Code)
	assert.Equal(t, 0, calls, "占用中的键不能被并发执行")
	assert.Equal(t, "1", recorder.Header().Get("Retry-After"))
	errObj := decodeIdempotencyError(t, recorder)
	assert.Equal(t, "idempotency_request_in_progress", errObj["code"])
}

// TestClientIdempotencyKeyExpiresAfterTTL 键有保留期：过期后同键可以重新开始。
func TestClientIdempotencyKeyExpiresAfterTTL(t *testing.T) {
	setupIdempotencyMiddlewareTest(t)

	calls := 0
	engine := newIdempotencyTestEngine(func(c *gin.Context) {
		calls++
		c.JSON(http.StatusOK, gin.H{"calls": calls})
	})

	body := `{"model":"gpt-4o","messages":[]}`
	require.Equal(t, http.StatusOK, postIdempotentRequest(t, engine, "idem-ttl", body).Code)
	require.Equal(t, 1, calls)

	// 把保留期推到过去，等价于「TTL 到了」。
	require.NoError(t, model.DB.Model(&model.IdempotencyKey{}).
		Where("idempotency_key = ?", "idem-ttl").
		Update("expires_at", common.GetTimestamp()-1).Error)

	second := postIdempotentRequest(t, engine, "idem-ttl", body)
	assert.Equal(t, http.StatusOK, second.Code)
	assert.Equal(t, 2, calls, "过期后的同键请求应该重新执行")
	assert.Empty(t, second.Header().Get(idempotencyReplayedHeader))
}

// TestClientIdempotencyKeyRejectsOversizedKey 超长键直接 400，不落库。
func TestClientIdempotencyKeyRejectsOversizedKey(t *testing.T) {
	setupIdempotencyMiddlewareTest(t)

	calls := 0
	engine := newIdempotencyTestEngine(func(c *gin.Context) {
		calls++
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	recorder := postIdempotentRequest(t, engine, strings.Repeat("k", model.IdempotencyKeyMaxLength+1), `{"a":1}`)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Equal(t, 0, calls)
	errObj := decodeIdempotencyError(t, recorder)
	assert.Equal(t, "invalid_idempotency_key", errObj["code"])
}

// TestClientIdempotencyKeyLeavesBodyReadable 摘要算完后处理器必须还能完整读到 body。
func TestClientIdempotencyKeyLeavesBodyReadable(t *testing.T) {
	setupIdempotencyMiddlewareTest(t)

	var seen []string
	engine := newIdempotencyTestEngine(func(c *gin.Context) {
		storage, err := common.GetBodyStorage(c)
		require.NoError(t, err)
		_, err = storage.Seek(0, io.SeekStart)
		require.NoError(t, err)
		data, err := io.ReadAll(storage)
		require.NoError(t, err)
		seen = append(seen, string(data))
		c.JSON(http.StatusOK, gin.H{"bytes": len(data)})
	})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	first := postIdempotentRequest(t, engine, "idem-body", body)
	require.Equal(t, http.StatusOK, first.Code)
	second := postIdempotentRequest(t, engine, "idem-body", body)
	require.Equal(t, http.StatusOK, second.Code)

	assert.Equal(t, []string{body}, seen, "处理器读到的 body 必须与原始请求一致，且回放时不再执行")
	assert.Equal(t, first.Body.String(), second.Body.String())
}

// TestClientIdempotencyKeySeparatedByToken 幂等域按令牌隔离：同一用户换令牌是不同的业务。
func TestClientIdempotencyKeySeparatedByToken(t *testing.T) {
	setupIdempotencyMiddlewareTest(t)

	calls := 0
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		tokenId := 7
		if c.GetHeader("X-Test-Token") == "other" {
			tokenId = 8
		}
		c.Set("id", 42)
		c.Set("token_id", tokenId)
		c.Next()
	})
	engine.Use(ClientIdempotencyKey())
	engine.POST(idempotencyTestPath, func(c *gin.Context) {
		calls++
		c.JSON(http.StatusOK, gin.H{"calls": calls})
	})

	body := `{"model":"gpt-4o","messages":[]}`
	require.Equal(t, http.StatusOK, postIdempotentRequest(t, engine, "idem-token-scope", body).Code)

	req := httptest.NewRequest(http.MethodPost, idempotencyTestPath, strings.NewReader(body))
	req.Header.Set("X-Test-Token", "other")
	req.Header.Set(IdempotencyKeyHeader, "idem-token-scope")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, 2, calls, "换令牌后同一个键是另一笔业务，应该执行")
	assert.Empty(t, recorder.Header().Get(idempotencyReplayedHeader))
}
