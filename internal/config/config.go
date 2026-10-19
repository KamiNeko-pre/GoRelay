package config

import (
	"log"
	"os"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
)

type Config struct {
	// Config 是 config.yaml 的内存映射。mapstructure 标签将 YAML 字段映射到 Go 字段。
	App       AppConfig       `mapstructure:"app"`
	MySQL     MySQLConfig     `mapstructure:"mysql"`
	Redis     RedisConfig     `mapstructure:"redis"`
	Publisher PublisherConfig `mapstructure:"publisher"`
	Engine    EngineConfig    `mapstructure:"engine"`
}

type AppConfig struct {
	// AppConfig 保存服务自身的监听端口、可选代理和 pprof 配置。
	Port      int    `mapstructure:"port"`
	Proxy     string `mapstructure:"proxy"`
	PprofPort int    `mapstructure:"pprof_port"`
}
type MySQLConfig struct {
	// MySQLConfig 用于控制连接地址及连接池容量。
	DSN          string `mapstructure:"dsn"`
	MaxIdleConns int    `mapstructure:"max_idle_conns"`
	MaxOpenConns int    `mapstructure:"max_open_conns"`
}

type RedisConfig struct {
	Addr            string `mapstructure:"addr"`
	Password        string `mapstructure:"password"`
	DB              int    `mapstructure:"db"`
	Stream          string `mapstructure:"stream"`
	ConsumerGroup   string `mapstructure:"consumer_group"`
	ClaimMinIdleMS  int    `mapstructure:"claim_min_idle_ms"`
	ClaimIntervalMS int    `mapstructure:"claim_interval_ms"`
}

// PublisherConfig controls the standalone Outbox delivery process.
type PublisherConfig struct {
	BatchSize           int `mapstructure:"batch_size"`
	PollIntervalMS      int `mapstructure:"poll_interval_ms"`
	BatchTimeoutSeconds int `mapstructure:"batch_timeout_seconds"`
}

type EngineConfig struct {
	// EngineConfig 决定 Worker 数量以及网络和 Lua 执行的超时阈值。
	WorkerCount       int `mapstructure:"worker_count"`
	LuaTimeout        int `mapstructure:"lua_timeout"`
	HTTPTimeoutDirect int `mapstructure:"http_timeout_direct"`
	HTTPTimeoutProxy  int `mapstructure:"http_timeout_proxy"`
}

var (
	// globalConfig 由 Get 提供给全局使用；读写配合 RWMutex，
	// 避免配置热更新时读到正在替换的指针。
	globalConfig *Config
	configMutex  sync.RWMutex
)

func InitConfig() {
	viper.SetEnvPrefix("LUA_SPIDER")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()
	bindEnvironment()
	viper.SetDefault("redis.claim_min_idle_ms", 120000)
	viper.SetDefault("redis.claim_interval_ms", 15000)
	viper.SetDefault("publisher.batch_size", 100)
	viper.SetDefault("publisher.poll_interval_ms", 1000)
	viper.SetDefault("publisher.batch_timeout_seconds", 10)
	// Viper 从项目根目录下的 configs/config.yaml 读取配置。
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("./configs")
	if path := os.Getenv("LUA_SPIDER_CONFIG"); path != "" {
		viper.SetConfigFile(path)
	}
	if err := viper.ReadInConfig(); err != nil {
		log.Fatalf("配置读取失败,请检查configs/config.yaml是否存在:%v", err)
	}
	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		log.Fatalf("配置数据装填失败: %v", err)
	}

	// 首次加载发生在服务启动阶段，此时尚不存在并发读取。
	globalConfig = &cfg

	// 监听配置文件变更。重新反序列化成功后整体替换配置，
	// 这样一次 Get 要么看到旧配置，要么看到完整的新配置。
	viper.WatchConfig()
	viper.OnConfigChange(func(e fsnotify.Event) {
		log.Printf("检测到配置文件变化: %s", e.Name)
		var newCfg Config
		if err := viper.Unmarshal(&newCfg); err != nil {
			log.Printf("配置热更新失败: %v\n", err)
			return
		}
		configMutex.Lock()
		globalConfig = &newCfg
		configMutex.Unlock()
		log.Println("配置文件热更新成功")
	})
}

// bindEnvironment makes deployment-only values such as a database DSN
// replaceable without putting credentials in the checked-in YAML file.
func bindEnvironment() {
	keys := []string{
		"app.port", "app.proxy", "app.pprof_port",
		"mysql.dsn", "mysql.max_idle_conns", "mysql.max_open_conns",
		"redis.addr", "redis.password", "redis.db", "redis.stream", "redis.consumer_group",
		"redis.claim_min_idle_ms", "redis.claim_interval_ms",
		"publisher.batch_size", "publisher.poll_interval_ms", "publisher.batch_timeout_seconds",
		"engine.worker_count", "engine.lua_timeout",
		"engine.http_timeout_direct", "engine.http_timeout_proxy",
	}
	for _, key := range keys {
		if err := viper.BindEnv(key); err != nil {
			log.Fatalf("环境变量绑定失败 %s: %v", key, err)
		}
	}
}

func Get() *Config {
	// 当前调用方只读取配置字段；若未来需要修改返回值，应改为返回副本，
	// 否则会绕开这把锁直接修改全局配置。
	configMutex.RLock()
	defer configMutex.RUnlock()
	return globalConfig
}
