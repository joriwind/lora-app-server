package common

import (
	"github.com/brocaar/loraserver/api/ns"
	"github.com/garyburd/redigo/redis"
	"github.com/jmoiron/sqlx"
	"github.com/joriwind/hecomm-api/hecommAPI"
	"github.com/joriwind/lora-app-server/internal/handler"
)

type Context struct {
	DB            *sqlx.DB
	RedisPool     *redis.Pool
	NetworkServer ns.NetworkServerClient
	Handler       handler.Handler
	Hecomm        *hecommAPI.Platform
}
