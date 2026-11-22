package controller

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// Liveness and readiness probes for ALB / ECS / Kubernetes.
//
// Why this exists (T4 fact table §3.3): the historical GET /api/status returns
// `success: true` as long as the process is up — it never touches the database,
// so an orchestrator pointed at it keeps routing traffic to an instance whose
// DB is gone. GET /api/status/test does check the DB but sits behind AdminAuth,
// which a load balancer cannot supply.
//
// Both handlers below are anonymous by design and expose no configuration.

const (
	// readinessDBTimeout bounds a single readiness DB probe so a black-holed
	// database cannot hang the load balancer health check loop.
	readinessDBTimeout = 3 * time.Second
	// readinessRedisTimeout bounds the optional Redis probe.
	readinessRedisTimeout = 2 * time.Second
)

// Healthz is the liveness probe: it answers "is this process able to serve
// HTTP at all". Deliberately dependency-free — a liveness probe that depends on
// the database turns a DB outage into a restart loop.
func Healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"version": common.Version,
	})
}

// Readyz is the readiness probe: it answers "can this instance actually serve
// traffic right now", which means the primary datastore must be reachable.
// Returns 200 when every required dependency answers, 503 otherwise.
func Readyz(c *gin.Context) {
	checks := make(map[string]string, 2)
	ready := true

	if err := pingDatabaseForReadiness(); err != nil {
		checks["database"] = "unavailable"
		ready = false
	} else {
		checks["database"] = "ok"
	}

	// Redis is optional (see common/redis.go: unset REDIS_CONN_STRING disables
	// it) so it is only reported, never required, unless explicitly demanded.
	switch {
	case !common.RedisEnabled || common.RDB == nil:
		checks["redis"] = "disabled"
	case redisIsRequiredForReadiness():
		if err := pingRedisForReadiness(); err != nil {
			checks["redis"] = "unavailable"
			ready = false
		} else {
			checks["redis"] = "ok"
		}
	default:
		if err := pingRedisForReadiness(); err != nil {
			checks["redis"] = "unavailable"
		} else {
			checks["redis"] = "ok"
		}
	}

	status := http.StatusOK
	state := "ready"
	if !ready {
		status = http.StatusServiceUnavailable
		state = "unavailable"
	}
	c.JSON(status, gin.H{
		"status":  state,
		"version": common.Version,
		"checks":  checks,
	})
}

// redisIsRequiredForReadiness lets deployments that rely on cross-instance rate
// limiting / caching treat Redis as a hard readiness dependency.
// Default: false (Redis optional, matching the code's own behaviour).
func redisIsRequiredForReadiness() bool {
	v := os.Getenv("READINESS_REQUIRE_REDIS")
	if v == "" {
		return false
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return false
	}
	return parsed
}

// pingDatabaseForReadiness bypasses model.PingDB's 10s success cache: a
// readiness probe must observe the current state, not a recent healthy sample.
func pingDatabaseForReadiness() error {
	if model.DB == nil {
		return errDatabaseNotInitialised
	}
	sqlDB, err := model.DB.DB()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), readinessDBTimeout)
	defer cancel()
	return sqlDB.PingContext(ctx)
}

func pingRedisForReadiness() error {
	ctx, cancel := context.WithTimeout(context.Background(), readinessRedisTimeout)
	defer cancel()
	return common.RDB.Ping(ctx).Err()
}

type databaseNotInitialisedError struct{}

func (databaseNotInitialisedError) Error() string { return "database not initialised" }

var errDatabaseNotInitialised error = databaseNotInitialisedError{}
