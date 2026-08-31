package cabi

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// awaitOutputResult 的 decision 核心：

func TestAwaitOutputResult_Success(t *testing.T) {
	res, err := awaitOutputResultWith(0, "qq", `{"x":1}`, func(pid int32, ch, args string) error {
		return nil
	}, outputSendTimeout)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	m, _ := res.(map[string]interface{})
	if m["status"] != "sent" {
		t.Fatalf("expected status=sent, got %v", m["status"])
	}
}

func TestAwaitOutputResult_Failure(t *testing.T) {
	_, err := awaitOutputResultWith(0, "qq", `{}`, func(pid int32, ch, args string) error {
		return errors.New("meta 中需要 group_id 或 user_id 字段")
	}, outputSendTimeout)
	if err == nil {
		t.Fatal("expected error on failed send, got nil (旧实现会谎报成功)")
	}
	if !strings.Contains(err.Error(), "需要 group_id") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAwaitOutputResult_Timeout(t *testing.T) {
	res, err := awaitOutputResultWith(0, "qq", `{}`, func(pid int32, ch, args string) error {
		time.Sleep(2 * time.Second) // 模拟插件发送迟迟不确认
		return nil
	}, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("unconfirmed 不应返回 error，got %v", err)
	}
	m, _ := res.(map[string]interface{})
	if m["status"] != "unconfirmed" {
		t.Fatalf("expected status=unconfirmed, got %v", m["status"])
	}
}
