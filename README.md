# 文化产品收益分账账本

纯后端分账账本：订单、退货、渠道费率、合同版本与结算确认按**审计序号**写入
不可变时间线，结算确认幂等地产出复式记账的分账指令。面向创作者与合作社的
分账争议，系统可以证明：

1. **原始结算没有被改写** —— 事件流是 sha256 哈希链，任何字段被改动都会断链；
   `events/settlements/postings/contracts` 由触发器禁止 UPDATE/DELETE。
2. **后续退货形成独立冲正** —— 退货在结算中生成独立的 `reversal` 腿分录，
   不与销售腿轧差，并按**原订单周期**的合同与费率冲回。
3. **合同切换日使用了正确版本** —— 每个周期按 `effective_from <= period` 选择
   最新版本；切换日（2026-09-10）当天销售用 v2，此前销售的退货仍按 v1 冲回；
   结算行内冻结 `contract_version` 与 `terms` 快照，故障恢复后解释不变。

技术栈：Go 1.25（仅 pgx 一个外部依赖）+ PostgreSQL 16 + Docker Compose。

## 快速开始

```bash
scripts/up.sh            # 构建并启动 db -> migrate -> seed -> app（含健康检查）
scripts/demo.sh          # 端到端演示：跨午夜退货、切换日、幂等确认、迟到事件、ACL、审计
scripts/fault_inject.sh  # 故障注入：崩溃结算事务，验证回滚/幂等重试/序号连续/合同解释保留
scripts/reset.sh         # 停止并清空持久卷（演示环境复位）
```

演示顺序：先在全新栈上跑 `demo.sh`，再跑 `fault_inject.sh`（追加新周期，不影响前者结论）。

离线完整性校验（哈希链 + 借贷平衡 + 重放对账，失败退出码非零）：

```bash
docker compose --profile audit run --rm verify
```

## 架构

```
cmd/server            子命令：migrate | seed | serve | verify
internal/ledger       纯函数领域核心（无 IO）：金额解析、周期归属、合同/费率选择、
                      分账计算、哈希链、重放快照与差异 —— 单测全覆盖
internal/store        PostgreSQL 访问层：所有多步写入在单事务内完成
internal/api          HTTP 接口与角色鉴权
internal/migrate      内嵌 SQL 迁移（compose 中的一次性服务）
internal/seed         contracts/ 种子：区域、参与方、合同版本与费率（作为事件上链）
contracts/            分账规则与合同版本（种子输入）
fixtures/             演示事件流（固定 event_id，可重复执行）
scripts/              启动、演示、故障注入、复位脚本
```

### 不变量

| 不变量 | 机制 |
| --- | --- |
| 时间线不可改写 | 哈希链（`prev_hash`/`hash`）+ 不可变触发器 + `verify` 校验 |
| 审计序号连续不回退 | `seq` 在 `chain_state` 行锁内分配，回滚不烧号，重启继续递增 |
| 确认幂等 | `confirmation_id` 唯一；重复确认返回原结算，指令只产生一次 |
| 并发结算平衡 | 周期咨询锁串行化 + 单事务写分录 + 提交前借=贷校验 |
| 跨午夜归属 | 周期 = `occurred_at` 按区域 IANA 时区折算的日历日 |
| 迟到事件可审计不回退 | `late=true` 保留在时间线但不计入结算；水位只 `GREATEST` 前进 |
| 参与方数据隔离 | 对账单只含本人分录与公开依据（合同版本、比例、来源事件序号） |

### 分账规则

金额全程以分（int64）计算，费率遵循 `contracts/分账规则.json` 的 `fee_scale=4`
（如 `0.0250` = 250 bps）。每笔订单：先扣渠道费，净额按合同 bps 在
创作者/合作社/平台间分配，floor 尾差归平台，四份之和恒等于订单额，
因此借贷天然平衡；退货按相同方式以负向（借记）独立成腿。

## API 一览

| 方法与路径 | 角色 | 说明 |
| --- | --- | --- |
| `GET /healthz` `/readyz` | 公开 | 存活/就绪探针 |
| `POST /v1/events` | admin | 写入事件（幂等键 `event_id`），返回 `seq/period/late/hash` |
| `POST /v1/settlements/confirm` | admin | 确认周期结算（幂等键 `confirmation_id`） |
| `GET /v1/statements?period=` | 参与方 | 自己的分录与公开依据 |
| `GET /v1/settlements/{id}` | admin/auditor | 结算明细（含冻结的合同解释） |
| `GET /v1/audit/events?from_seq&to_seq` | auditor | 事件流（含迟到标记与哈希） |
| `GET /v1/audit/snapshot?as_of_seq=` | auditor | 指定序号水位重算快照，并与账本对账 |
| `GET /v1/audit/diff?from_seq&to_seq` | auditor | 两次重算的差异（参与方增量 + 新结算） |
| `GET /v1/audit/verify-chain` | auditor | 哈希链校验 |
| `GET /v1/watermarks` | auditor | 各区域水位 |
| `POST /v1/admin/fault?mode=crash` | admin | 故障注入（需 `LEDGER_FAULT_HOOKS=1`） |

认证：`Authorization: Bearer <token>`。演示 token 见 `contracts/versions.json`
（`dev-admin-token` / `dev-auditor-token` / `dev-creator-token` 等，仅用于本地演示）。

确认接口支持测试钩子 `?fault=crash_before_commit|crash_after_commit`
（同样需 `LEDGER_FAULT_HOOKS=1`），用于演示事务回滚与幂等重试。

## 演示场景（fixtures/分账事件.json）

| 事件 | 发生时间（+08:00） | 归属周期 | 说明 |
| --- | --- | --- | --- |
| O-01 订单 800.00 | 09-09 23:50 | 2026-09-09 | 合同 v1（45/35/20） |
| O-02 订单 1200.00 | 09-10 00:10 | 2026-09-10 | 跨午夜，切换日用 v2（50/30/20） |
| R-01 退 O-01 800.00 | 09-10 00:05 | 2026-09-10 | 独立冲正腿，按原订单 v1 冲回 |
| O-03 订单 500.00 丝绸 | 09-10 08:00 | 2026-09-10 | 渠道费率 3% |
| 确认 09-09（两次同键） | — | — | 第二次为幂等 duplicate |
| O-00 订单 200.00 | 09-09 10:00 | 2026-09-09 | 周期已关闭才到达 → `late=true` |
| 确认 09-10（另键再确认） | — | — | 409 冲突，周期已被占用 |

关键数字：创作者净额 827.50 = 351.00（09-09）+ 585.00 + 242.50 − 351.00（09-10 冲正）；
合作社 496.50；渠道 45.00；平台收入户 331.00。

## 本地开发

```bash
go build ./...        # 需要 Go 1.25
go test ./internal/ledger/   # 领域核心单测（纯函数，无需数据库）
```

数据库交互层通过 `scripts/demo.sh` / `scripts/fault_inject.sh` 在
Docker Compose 栈上做端到端验证。
