# ReaderGo

基于 LLM 的异步视频理解服务基础设施。当前版本提供用户认证、视频任务创建、Kafka Worker 骨架与 PostgreSQL/pgvector 数据模型。

## 当前视频理解链路

第一版只使用带时间戳的语音转写与关键帧画面描述作为证据：

```text
视频 URL → 下载与存储 → 音频提取与语音转写
        → 镜头切分与代表帧提取 → 视觉理解
        → 时间轴对齐 → 摘要与向量索引 → 视频问答
```

视频片段的检索文本由 `transcript + visual_caption` 构成。画面文字识别不在当前处理链路中。

## 文件地形图

```text
ReaderGo/
│
├── README.md                 ← 项目说明、启动方式与当前文件地图
├── docs/
│   └── 设计方案_v1.md         ← 视频理解 Agent 的产品与技术设计依据
├── go.mod / go.sum           ← Go 模块定义与锁定的依赖版本
├── .env.example              ← 本地运行所需环境变量示例（不要提交真实密钥）
├── docker-compose.yml        ← 本地 PostgreSQL、Redis、Kafka、MinIO 编排
│
├── migrations/
│   └── 001_init.sql          ← PostgreSQL/pgvector 扩展、状态枚举和全部核心表结构
│
├── cmd/                      ← 可独立启动的应用入口
│   ├── api/main.go           ← HTTP API：加载配置、连接数据库/Kafka、启动 Gin 服务
│   └── worker/main.go        ← Kafka Worker：消费视频任务并处理优雅退出
│
└── internal/                 ← 仅供本项目内部使用的业务实现
    ├── api/server.go         ← 路由与接口：注册、登录、视频、会话、消息
    ├── auth/auth.go          ← bcrypt 密码哈希、JWT 签发与 Bearer Token 鉴权中间件
    ├── config/config.go      ← .env / 环境变量加载、必填配置校验
    ├── db/db.go              ← GORM PostgreSQL 连接创建
    ├── model/models.go       ← 用户、视频、任务、摘要、会话和消息的数据模型
    ├── queue/queue.go        ← Kafka 视频任务消息定义与生产者封装
    └── worker/worker.go      ← 消费任务、维护处理状态机、衔接可替换的视频 AI 处理器
```

请求处理主路径：`cmd/api → internal/api → internal/auth / internal/db → internal/queue → Kafka → cmd/worker → internal/worker`。

## 启动

```powershell
Copy-Item .env.example .env
docker compose up -d
go mod tidy
go run ./cmd/api
```

另开终端启动消费者：`go run ./cmd/worker`。

接口前缀为 `/api`。先调用 `POST /api/auth/register`，再以返回的 Bearer Token 调用视频接口。

> `.env` 中的 JWT_SECRET 必须在部署前改为高熵私密值；修改它会使所有现有 Token 失效。
