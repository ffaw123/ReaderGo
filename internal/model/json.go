package model

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// JSONDocument 为 JSONB 字段提供数据库读写与正确的 HTTP JSON 序列化。
// 直接使用 []byte 会被 encoding/json 编码成 Base64，无法满足前端数组/对象契约。
type JSONDocument []byte

func NewJSONDocument(value any) JSONDocument {
	encoded, err := json.Marshal(value)
	if err != nil {
		return JSONDocument("null")
	}
	return JSONDocument(encoded)
}

func (j JSONDocument) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	if !json.Valid(j) {
		return nil, fmt.Errorf("invalid JSON document")
	}
	return j, nil
}

func (j *JSONDocument) UnmarshalJSON(value []byte) error {
	if !json.Valid(value) {
		return fmt.Errorf("invalid JSON document")
	}
	*j = append((*j)[:0], value...)
	return nil
}

func (j JSONDocument) Value() (driver.Value, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	if !json.Valid(j) {
		return nil, fmt.Errorf("invalid JSON document")
	}
	return []byte(j), nil
}

func (j *JSONDocument) Scan(value any) error {
	switch raw := value.(type) {
	case nil:
		*j = nil
		return nil
	case []byte:
		*j = append((*j)[:0], raw...)
	case string:
		*j = append((*j)[:0], raw...)
	default:
		return fmt.Errorf("scan JSON document from %T", value)
	}
	if len(*j) > 0 && !json.Valid(*j) {
		return fmt.Errorf("database returned invalid JSON document")
	}
	return nil
}
