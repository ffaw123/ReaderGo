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
├── docker-compose.yml              ← PostgreSQL、Redis、RabbitMQ、MinIO 编排
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
│   ├── RabbitMQ设计方案_v1.md      ← 当前 RabbitMQ、Outbox 与任务租约设计
│   ├── Kafka到RabbitMQ迁移指南.md  ← 消息层迁移约束与验收清单
│   └── RAG设计指南.md               ← 双路召回、RRF 融合与证据引用设计
├── migrations/
│   ├── 001_init.sql                ← 核心表、状态枚举与 pgvector 初始化
│   ├── 002_upload_sessions.sql     ← 本地分片上传会话表
│   ├── 003_asr_segments.sql        ← ASR 原始转写片段表
│   └── 004_ocr_segments.sql        ← OCR 可用抽样画面文字表
│
├── cmd/                            ← 可独立启动的应用入口
│   ├── api/main.go                 ← HTTP API 服务入口，只写业务数据与 Outbox
│   ├── outbox/main.go              ← Outbox Dispatcher 与 Confirm Publisher
│   └── worker/main.go              ← RabbitMQ 消费、任务执行和租约回收入口
│
├── internal/                       ← 后端业务实现
│   ├── api/server.go                ← 路由、鉴权、视频和上传接口
│   ├── auth/auth.go                 ← 密码哈希、JWT 签发与鉴权中间件
│   ├── config/config.go             ← 环境变量加载与配置校验
│   ├── db/db.go                     ← GORM PostgreSQL 连接
│   ├── model/models.go              ← 数据模型和状态、任务常量
│   ├── outbox/                      ← 事务 Outbox 写入和可靠分发
│   ├── queue/                       ← RabbitMQ 契约、拓扑、Confirm 发布与手动 ACK 消费
│   ├── storage/multipart.go         ← MinIO 分片上传封装
│   └── worker/                       ← ASR、OCR、RAG 分段、Embedding、章节摘要与任务编排
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

请求处理主路径：`cmd/api → internal/api → PostgreSQL(message_outbox) → cmd/outbox → RabbitMQ → cmd/worker → internal/worker`。

`asr` 会按 60 秒非重叠片段并发提取音频并调用转写接口，`ASR_CONCURRENCY` 默认和上限均为 10。每个 WAV 在请求模型前通过 FFmpeg `volumedetect` 检查峰值音量，不高于 `-50 dB` 的近似全静音片段会直接跳过；无音轨视频作为合法的空 ASR 结果成功完成。所有非静音片段成功后才原子替换该视频的原始 ASR 结果。

`vision` 会按 15 秒固定采样和场景变化采样抽帧，感知哈希去重后通过百炼 OpenAI 兼容接口调用 `qwen-vl-ocr`。使用 `OCR_API_KEY`、`OCR_BASE_URL` 和 `OCR_MODEL` 配置独立服务，`OCR_CONCURRENCY` 默认和上限均为 10。提示词要求模型仅提取清晰可见文字，无文字时固定返回 `NO_TEXT`，并禁止猜测或输出 Markdown/LaTeX。Qwen-VL-OCR 不返回置信度，因此落库前会丢弃空文本、无文字占位回复、纯标点/格式标记和不足两个有效字符的低价值结果；`ocr_segments` 不保存处理状态或置信度。

`rag_index` 会在 ASR 与 OCR 均成功后，按 60 秒非重叠窗口生成检索摘要、关键事实、关键词和检索文本，并在短事务中原子替换 `video_segments`。每个窗口独立调用结构化生成接口，默认通过 `RAG_CONCURRENCY=10` 限制并发。`rag_index` 成功后再并行创建 `embedding` 与 `summary`：Embedding 按每批最多 10 个片段请求 1024 维向量并原子替换 `segment_embeddings`；Summary 同时读取窗口的结构化理解与原始 ASR/OCR，按内容发展合并相邻单元，生成“总体概览 + 语义章节 + 章节要点”的 Markdown 文档，不向用户暴露 60 秒技术边界。Summary 任务一旦成功，前端即可提前预览和下载 `.md`；视频仍需 Embedding 等全部阶段成功后才显示为 `ready`。

RAG 分段、Embedding 和 Summary 共用 `RAG_API_KEY`、`RAG_BASE_URL` 与 `RAG_REQUEST_TIMEOUT`；分段与 Summary 使用 `RAG_CHAT_MODEL`，Embedding 使用 `RAG_EMBEDDING_MODEL`。模型 HTTP 临时错误由 SDK 最多重试 3 次；HTTP 成功但结构或完整性校验失败时按 500ms、1s 在当前任务内额外重试 2 次。Summary 连续三次返回无效结构后直接进入 DLQ，避免再叠加任务级模型调用。

模型接口的 `429`、超时、网络错误和 `5xx` 会继续进入任务级重试，重试耗尽后任务失败并投递 DLQ；`401`、`403`、无效参数（通常为 `400/422`）和模型不存在（通常为 `404`）会立即将任务标记为失败并投递 DLQ，不消耗任务级重试。若 ASR 与 OCR 均未产生有效文字，RAG 会直接把视频标记为失败并提示用户上传包含清晰语音或可识别文字的视频，此类用户输入问题不会创建下游任务或进入 DLQ。

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

脚本会启动 Docker 依赖（PostgreSQL、RabbitMQ、MinIO 和 Redis），并分别打开 Outbox Dispatcher、Worker、API、前端四个终端。首次运行时，只有在 `web/node_modules` 不存在的情况下才会执行 `npm install`。启动 Worker 前需要在 `.env` 中填写独立的 ASR、OCR 和 RAG 模型凭据。前端默认访问地址为 <http://localhost:5173>，API 地址为 <http://localhost:8080>，RabbitMQ 管理界面为 <http://localhost:15672>。

### 手动启动

需要逐项排查启动问题时，可分别运行以下命令：

```powershell
# 终端 1：项目根目录
docker compose up -d
go run ./cmd/api

# 终端 2：项目根目录（必须启动，负责把事务 Outbox 发布到 RabbitMQ）
go run ./cmd/outbox

# 终端 3：项目根目录
go run ./cmd/worker

# 终端 4：web 目录
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

视频后台处理支持 `POST /api/videos/:id/pause` 与 `POST /api/videos/:id/resume`。暂停以任务阶段为粒度：已完成阶段会保留，正在运行的阶段安全停止后回到 `pending`，恢复时从该阶段开头重新执行。

> `.env` 中的 JWT_SECRET 必须在部署前改为高熵私密值；修改它会使所有现有 Token 失效。
