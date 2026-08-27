# 离线检索评测说明

本目录保存离线检索评测使用的冻结数据库、问题集和运行结果。评测分为两个阶段：先使用调参集选择参数，再固定参数并在正式集上验证。正式集只用于最终验证，不应根据正式集结果继续调参。

## 1. 文件说明

- `retrieval_tuning_database.sql`：调参集数据库快照。
- `retrieval_tuning_questions.jsonl`：调参集问题，共 15 个视频、120 题。
- `retrieval_evaluation_database.sql`：正式集数据库快照。
- `retrieval_evaluation_questions.jsonl`：正式集问题，共 10 个视频、80 题。
- `retrieval_eval_runs.jsonl`：每次完整评测的汇总和全量运行记录，每次运行追加一行。
- `retrieval_eval_results/<run_id>.jsonl`：本次运行的逐题详细结果，便于检查具体召回窗口。

数据库快照和问题集必须配套使用，不能把调参问题集连接到正式数据库，或反过来使用。

## 2. 问题集结构与注意事项

问题集采用 JSONL 格式，一行代表一道问题，主要字段如下：

```json
{
  "question_id": "v01_q01",
  "tenant_id": "租户 ID",
  "video_id": "视频 ID",
  "video_title": "便于人工查看的视频名称",
  "question": "经过自然改写的问题",
  "answer": "人工参考答案",
  "tags": ["semantic", "math"],
  "gold_evidence_groups": [["segment-id-1", "segment-id-2"], ["segment-id-3"]]
}
```

`answer` 用于人工检查，实际检索指标根据 `gold_evidence_groups` 中的窗口 ID 计算。

- 同一个内层数组表示一组等价答案窗口，命中其中任意一个即可命中该证据点。
- 不同内层数组表示不同的必要证据点。Recall@5 按前 5 个结果覆盖了多少组证据计算。
- MRR@5 根据前 5 个结果中第一个正确窗口的排名计算，例如第 1 名命中为 1，第 2 名命中为 1/2。
- Gold 窗口必须属于该题的 `tenant_id` 和 `video_id`，同一个窗口不能跨证据组重复。
- 问题需要保留必要的专有名词、公式或数值线索，但不要直接照抄 `retrieval_text`。
- 一个问题存在多个独立证据点或多个等价窗口时，应完整标注，避免漏标导致正确召回被判错。
- Segment ID 来自配套 SQL 快照。如果重新解析或重新索引视频导致 ID 变化，需要同时更新数据库快照和问题集。

## 3. 完整评测中的实验组

完整评测一次会运行下面 13 组实验：

| 实验组 | 意义 |
| --- | --- |
| `dense_only` | 只使用 Dense Vector，是主要对比基线。 |
| `bm25_only` | 只使用 transcript 和 OCR 的 BM25，是词法检索基线。 |
| `dense_bm25_equal` | Dense 与 BM25 使用 1:1 权重，观察等权融合效果。 |
| `dense_bm25_0_5` | Dense:BM25 为 1:0.5。 |
| `dense_bm25_0_25` | Dense:BM25 为 1:0.25。 |
| `dense_bm25_0_1` | Dense:BM25 为 1:0.1，观察弱词法信号的效果。 |
| `dense_bm25_k_5` | 固定 Dense:BM25=1:0.25，测试 RRF k=5。 |
| `dense_bm25_k_10` | 固定权重，测试 RRF k=10。 |
| `dense_bm25_k_30` | 固定权重，测试 RRF k=30。 |
| `dense_bm25_k_60` | 固定权重，测试经典 RRF k=60。 |
| `dense_bm25_raw` | Dense 与原始 transcript/OCR BM25 融合。 |
| `dense_bm25_retrieval` | Dense 与只检索 `retrieval_text` 的 BM25 融合。 |
| `dense_bm25_hybrid_fields` | Dense 与字段加权 BM25 融合；同时使用 `retrieval_text`、过滤后的 transcript 和 OCR。 |

其中 `dense_bm25_0_25`、`dense_bm25_k_10` 和 `dense_bm25_raw` 当前参数相同，分别保留在权重、k 值和 BM25 文本来源三组消融中，方便横向阅读。

## 4. 调参与正式评测流程

### 第一步：调整 Dense 与 BM25 的 RRF 权重

双击运行：

```text
batch\eval_tuning_start.bat
```

脚本会恢复调参数据库并运行全部实验。先比较下面几组：

```text
dense_only
bm25_only
dense_bm25_equal
dense_bm25_0_5
dense_bm25_0_25
dense_bm25_0_1
```

同时参考 Recall@5 和 MRR@5，选择对 Dense-only 提升较稳定的权重。当前选择为 Dense:BM25=`1:0.25`。

### 第二步：调整 RRF k 值

继续查看同一次调参运行中的：

```text
dense_bm25_k_5
dense_bm25_k_10
dense_bm25_k_30
dense_bm25_k_60
```

这些实验固定 Dense:BM25=`1:0.25`，只改变 k。当前选择为 `k=10`。

### 第三步：调整 BM25 文本来源和字段权重

双击运行：

```text
batch\eval_bm25_start.bat
```

该脚本使用调参数据库，比较以下 BM25-only 实验：

```text
bm25_raw
bm25_retrieval
bm25_hybrid_fields
bm25_hybrid_0_5_0_5
bm25_hybrid_0_25_0_25
bm25_hybrid_0_25_0_1
bm25_hybrid_0_1_0_1
```

前三组用于比较原始 ASR/OCR、`retrieval_text` 和混合字段；后四组用于降低 transcript、OCR 相对 `retrieval_text` 的权重。该测试只在窗口打印汇总，不写入结果文件。

当前字段权重选择为：

```text
retrieval_text : transcript : ocr_text = 1 : 0.25 : 0.25
```

随后用完整调参评测中的 `dense_bm25_raw`、`dense_bm25_retrieval` 和 `dense_bm25_hybrid_fields`，确认字段选择加入 Dense 后仍然有效。

### 第四步：固定参数，在正式集上验证

参数确定后不再根据正式集修改权重。双击运行：

```text
batch\eval_evaluation_start.bat
```

正式脚本会自动切换到正式数据库和正式问题集。最终重点比较：

```text
dense_only
dense_bm25_hybrid_fields
```

当前冻结参数为：

```text
Dense:BM25 = 1:0.25
RRF k = 10
BM25 字段权重 = 1:0.25:0.25
最终输出 Top 5
```

评测完成后窗口会保留，汇总结果追加到 `retrieval_eval_runs.jsonl`，逐题结果写入 `retrieval_eval_results` 目录。需要关闭评测数据库并删除临时数据卷时，运行：

```text
batch\eval_stop.bat
```

评测容器使用独立数据库，不会修改业务 PostgreSQL 数据卷。
