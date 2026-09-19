-- 0001_init.sql — 文化产品收益分账账本核心 schema (PostgreSQL 16)
--
-- 设计不变量：
--   1. events 是唯一事实来源：订单、退货、渠道费率、合同版本、结算确认全部按
--      seq（审计序号）写入，prev_hash/hash 构成哈希链，任何改写都会断链。
--   2. events / settlements / postings / contracts 禁止 UPDATE 与 DELETE（触发器）。
--   3. watermarks 只允许单调前进：closed_period 与 high_seq 用 GREATEST 更新，
--      迟到旧事件可以写入（late=true，可审计）但绝不回退水位。
--   4. postings 是复式记账：每行借贷二选一，同一 settlement 内借方合计必须等于
--      贷方合计（应用层在同一事务内校验，v_settlement_balance 供审计复核）。

CREATE TABLE chain_state (
    id        smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    last_seq  bigint NOT NULL DEFAULT 0,
    last_hash bytea  NOT NULL DEFAULT '\x00'::bytea
);
INSERT INTO chain_state (id) VALUES (1);

CREATE TABLE regions (
    code     text PRIMARY KEY,
    timezone text NOT NULL            -- IANA 时区，如 Asia/Shanghai；周期归属按它计算
);

CREATE TABLE participants (
    id           text PRIMARY KEY,    -- 'creator:li' / 'coop:tea' / 'channel:mall' / 'platform:ops'
    kind         text NOT NULL CHECK (kind IN ('creator','cooperative','channel','platform','auditor','admin')),
    display_name text NOT NULL,
    token_hash   bytea NOT NULL       -- sha256(API token)；明文只存在于客户端
);

-- 合同版本投影：由 contract_version 事件在写入同一时间线时投影生成，便于查询。
-- 结算时冻结使用版本的 terms 快照到 settlements.terms，故障恢复后解释不变。
CREATE TABLE contracts (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    contract_no    text   NOT NULL,
    version        int    NOT NULL,
    effective_from date   NOT NULL,   -- 切换日：period >= effective_from 启用本版本
    terms          jsonb  NOT NULL,   -- {"creator_bps":..,"coop_bps":..,"platform_bps":..} 合计 10000
    registered_seq bigint NOT NULL,   -- 来源事件 seq
    UNIQUE (contract_no, version)
);

CREATE TABLE events (
    seq         bigint       PRIMARY KEY,                         -- 审计序号：应用在 chain_state 行锁内分配，无空洞、重启不回退
    event_id    uuid        NOT NULL UNIQUE,                      -- 客户端幂等键
    type        text        NOT NULL CHECK (type IN
                  ('order','return','channel_rate','contract_version','settlement_confirmation')),
    region_code text        NOT NULL REFERENCES regions(code),
    order_id    text,                                             -- 订单/退货的业务关联键
    occurred_at timestamptz NOT NULL,                             -- 业务发生时间（带原始偏移）
    period      date        NOT NULL,                             -- 按 region 时区折算的归属周期（日）
    late        boolean     NOT NULL DEFAULT false,               -- 周期已关闭才到达 -> 迟到，仅审计
    payload     jsonb       NOT NULL,
    prev_hash   bytea       NOT NULL,
    hash        bytea       NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX events_region_period_idx ON events (region_code, period) WHERE type IN ('order','return');
CREATE INDEX events_type_idx          ON events (type);
CREATE INDEX events_order_idx         ON events (order_id) WHERE order_id IS NOT NULL;

CREATE TABLE watermarks (
    region_code   text PRIMARY KEY REFERENCES regions(code),
    closed_period date   NOT NULL DEFAULT DATE '1970-01-01',  -- <= 该日的周期已关闭
    high_seq      bigint NOT NULL DEFAULT 0                   -- 已折入确认结算的最大审计序号
);

CREATE TABLE settlements (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    confirmation_id  uuid   NOT NULL UNIQUE,   -- 重复确认 -> 同一行，分账指令只生成一次
    region_code      text   NOT NULL REFERENCES regions(code),
    period           date   NOT NULL,
    contract_no      text   NOT NULL,          -- 冻结的合同解释（切换日正确版本的证据）
    contract_version int    NOT NULL,
    terms            jsonb  NOT NULL,
    confirmation_seq bigint NOT NULL REFERENCES events(seq),
    gross_cents      bigint NOT NULL,
    returned_cents   bigint NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (region_code, period)               -- 每个区域每周期至多一笔有效结算
);

CREATE TABLE postings (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    settlement_id  bigint NOT NULL REFERENCES settlements(id),
    entry_no       int    NOT NULL,
    leg            text   NOT NULL CHECK (leg IN ('sale','reversal')),  -- 退货是独立冲正腿，不与销售净额轧差
    account        text   NOT NULL,
    participant_id text   NOT NULL REFERENCES participants(id),
    debit_cents    bigint NOT NULL DEFAULT 0,
    credit_cents   bigint NOT NULL DEFAULT 0,
    basis          jsonb  NOT NULL,   -- 公开依据：合同号/版本、适用 bps、来源事件 seq 列表
    CHECK ((debit_cents > 0) <> (credit_cents > 0)),
    UNIQUE (settlement_id, entry_no)
);
CREATE INDEX postings_participant_idx ON postings (participant_id);

-- ---- 不可变性 ---------------------------------------------------------------
CREATE OR REPLACE FUNCTION reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger table "%" is immutable (no %)', TG_TABLE_NAME, TG_OP;
END;
$$;

CREATE TRIGGER events_immutable      BEFORE UPDATE OR DELETE ON events      FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER settlements_immutable BEFORE UPDATE OR DELETE ON settlements FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER postings_immutable    BEFORE UPDATE OR DELETE ON postings    FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER contracts_immutable   BEFORE UPDATE OR DELETE ON contracts   FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- ---- 审计复核视图 -----------------------------------------------------------
CREATE VIEW v_settlement_balance AS
SELECT settlement_id,
       SUM(debit_cents)  AS debits,
       SUM(credit_cents) AS credits,
       SUM(debit_cents) - SUM(credit_cents) AS imbalance
FROM postings
GROUP BY settlement_id;
