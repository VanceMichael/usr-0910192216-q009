package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// CanonicalPayload 规范化事件载荷：JSON 键排序、数字保留原始字面量
// （UseNumber 避免 "800.00" 被浮点化为 800），保证任何时刻重算哈希一致。
func CanonicalPayload(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("canonical payload: %w", err)
	}
	return json.Marshal(v)
}

// HashRecord 是参与哈希链计算的字段集合。
type HashRecord struct {
	Seq        int64
	EventID    string
	Type       string
	RegionCode string
	OrderID    string
	OccurredAt time.Time
	Period     Date
	Late       bool
	Payload    []byte // 已规范化的载荷
}

// EventHash 计算链式哈希：sha256(prev_hash || 各字段长度前缀编码)。
// 任何字段被改写都会使本事件及之后全部哈希失效——
// 这是"原始结算没有被改写"的可验证证据。
func EventHash(prev []byte, rec HashRecord) []byte {
	h := sha256.New()
	writeSeg := func(b []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	writeSeg(prev)
	writeSeg([]byte(strconv.FormatInt(rec.Seq, 10)))
	writeSeg([]byte(rec.EventID))
	writeSeg([]byte(rec.Type))
	writeSeg([]byte(rec.RegionCode))
	writeSeg([]byte(rec.OrderID))
	writeSeg([]byte(rec.OccurredAt.UTC().Format(time.RFC3339Nano)))
	writeSeg([]byte(string(rec.Period)))
	if rec.Late {
		writeSeg([]byte("1"))
	} else {
		writeSeg([]byte("0"))
	}
	writeSeg(rec.Payload)
	return h.Sum(nil)
}

// VerifyChain 从 seed（创世 prev_hash）顺序校验整条事件链，
// 返回链头哈希。任一环节不符即报错并指出 seq。
func VerifyChain(events []Event, seed []byte) ([]byte, error) {
	prev := seed
	for _, ev := range events {
		if !bytes.Equal(ev.PrevHash, prev) {
			return nil, fmt.Errorf("prev_hash broken at seq %d", ev.Seq)
		}
		cp, err := CanonicalPayload(ev.Payload)
		if err != nil {
			return nil, fmt.Errorf("event %d: %w", ev.Seq, err)
		}
		want := EventHash(prev, HashRecord{
			Seq: ev.Seq, EventID: ev.EventID, Type: ev.Type,
			RegionCode: ev.RegionCode, OrderID: ev.OrderID,
			OccurredAt: ev.OccurredAt, Period: ev.Period, Late: ev.Late,
			Payload: cp,
		})
		if !bytes.Equal(want, ev.Hash) {
			return nil, fmt.Errorf("hash mismatch at seq %d", ev.Seq)
		}
		prev = ev.Hash
	}
	return prev, nil
}
