# ReaderGo

基于 LLM 的异步视频理解服务基础设施。

## 项目亮点
1. 文件分片上传：将文件拆分为分片，每次并发上传一组分片。优点：一次失败只重传失败的那一个片，而不需要从头重传整个大文件。并且假如不分片时上传大文件遇到网络波动，只有 n 次重试机会；而分片上传时每个分片有 n 次重试机会，提升上传的健壮性。同时大文件由于上传时间长，遇到网络波动概率更大。

## 文件地形图

```text
ReaderGo/
│
├── README.md                       ← 项目说明、启动方式与当前文件地图
├── .env.example                    ← 本地环境变量示例
├── docker-compose.yml              ← PostgreSQL、Redis、Kafka、MinIO 编排
├── openapi.json                    ← HTTP API 契约
├── go.mod / go.sum                 ← Go 模块与依赖锁定文件
│
├── batch/
│   ├── start.bat                   ← Windows 一键启动 Docker、API、Worker 和前端
│   ├── stop.bat                    ← 停止 Docker 与项目服务，保留数据卷
│   └── stop-v.bat                  ← 停止 Docker 与项目服务并删除数据卷
├── docker/
│   └── minio-cors.json             ← MinIO 浏览器直传的 CORS 配置
├── docs/
│   ├── 设计方案_v1.md              ← 视频理解 Agent 的产品与技术设计
│   └── kafka设计方案_v1.md         ← Kafka 异步处理与状态管理设计
├── migrations/
│   ├── 001_init.sql                ← 核心表、状态枚举与 pgvector 初始化
│   └── 002_upload_sessions.sql     ← 本地分片上传会话表
│
├── cmd/                            ← 可独立启动的应用入口
│   ├── api/main.go                 ← HTTP API 服务入口-有自己的生产者实例
│   └── worker/main.go              ← 创建 worker 实例，一个 worker 实例既有消费者也有生产者实例
│
├── internal/                       ← 后端业务实现
│   ├── api/server.go                ← 路由、鉴权、视频和上传接口
│   ├── auth/auth.go                 ← 密码哈希、JWT 签发与鉴权中间件
│   ├── config/config.go             ← 环境变量加载与配置校验
│   ├── db/db.go                     ← GORM PostgreSQL 连接
│   ├── model/models.go              ← 数据模型和状态、任务常量
│   ├── queue/queue.go               ← Kafka 消息和分阶段 topic
│   ├── storage/multipart.go         ← MinIO 分片上传封装
│   └── worker/worker.go             ← Worker 实例运行的具体逻辑-包括任务流程编排
│
└── web/                            ← Vue 3 + Vite 前端
    ├── package.json                 ← 前端依赖与脚本
    ├── vite.config.js               ← Vite 开发代理配置
    └── src/
        ├── views/                   ← 登录页与工作台页面
        ├── services/api.js          ← 后端 API 与分片上传客户端
        ├── lib/demo.js              ← 预览模式示例数据
        ├── App.vue / main.js        ← 前端应用入口
        └── styles.css               ← 全局样式
```

请求处理主路径：`cmd/api → internal/api → internal/auth / internal/db → internal/queue → Kafka → cmd/worker → internal/worker`。

## 启动

### 环境要求

- Docker
- Go 1.26 或更高版本
- Node.js 18 或更高版本

### Windows 一键启动

首次使用时，在项目根目录复制配置文件：

```powershell
Copy-Item .env.example .env
```

然后双击运行 [`batch/start.bat`](batch/start.bat)，或在 PowerShell 中执行：

```powershell
.\batch\start.bat
```

脚本会启动 Docker 依赖，并分别打开 API、Worker、前端三个终端。首次运行时，只有在 `web/node_modules` 不存在的情况下才会执行 `npm install`。前端默认访问地址为 <http://localhost:5173>，API 地址为 <http://localhost:8080>。

### 手动启动

需要逐项排查启动问题时，可分别运行以下命令：

```powershell
# 终端 1：项目根目录
docker compose up -d
go run ./cmd/api

# 终端 2：项目根目录
go run ./cmd/worker

# 终端 3：web 目录
cd web
npm install  # 仅首次安装依赖或依赖变更后需要
npm run dev
```

仅体验前端时，可在登录页选择“预览完整工作台”。

### 停止与清理

| 脚本 | 作用 |
| --- | --- |
| [`batch/stop.bat`](batch/stop.bat) | 依次关闭一键启动的前端、Worker、API PowerShell 终端，再执行 `docker compose down`；保留 PostgreSQL、MinIO 等数据卷。 |
| [`batch/stop-v.bat`](batch/stop-v.bat) | 依次关闭一键启动的前端、Worker、API PowerShell 终端，再执行 `docker compose down -v` 并删除项目数据卷。 |

> 使用 `stop-v.bat` 前请确认不需要保留本地数据，该操作无法恢复已删除的数据卷。

### 构建前端

```powershell
cd web
npm run build
```

构建产物输出到 `web/dist/`。分离部署时，可复制 `web/.env.example` 为 `web/.env.production`，并通过 `VITE_API_BASE_URL` 设置后端 API 地址。

接口前缀为 `/api`。先调用 `POST /api/auth/register`，再以返回的 Bearer Token 调用视频接口。

> `.env` 中的 JWT_SECRET 必须在部署前改为高熵私密值；修改它会使所有现有 Token 失效。
