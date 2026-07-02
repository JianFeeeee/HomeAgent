package onebot

import (
	"encoding/json"
	"testing"
)

func TestMessageText(t *testing.T) {
	seg := MessageText("hello")
	if seg.Type != "text" {
		t.Errorf("expected type 'text', got %q", seg.Type)
	}
	if seg.Data["text"] != "hello" {
		t.Errorf("expected data.text 'hello', got %q", seg.Data["text"])
	}
}

func TestMessageImage(t *testing.T) {
	seg := MessageImage("test.jpg")
	if seg.Type != "image" {
		t.Errorf("expected 'image', got %q", seg.Type)
	}
	if seg.Data["file"] != "test.jpg" {
		t.Errorf("expected 'test.jpg', got %q", seg.Data["file"])
	}
}

func TestMessageAt(t *testing.T) {
	seg := MessageAt(123456)
	if seg.Type != "at" {
		t.Errorf("expected 'at', got %q", seg.Type)
	}
	if seg.Data["qq"] != "123456" {
		t.Errorf("expected '123456', got %q", seg.Data["qq"])
	}
}

func TestEventMarshal(t *testing.T) {
	evt := Event{
		Time:     1234567890,
		SelfID:   10001,
		PostType: "message",
		MessageType: "group",
		GroupID:  999,
		UserID:   777,
		RawMessage: "hello",
		Sender: &Sender{
			UserID:   777,
			Nickname: "TestUser",
			Role:    "member",
		},
	}

	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatal(err)
	}

	var decoded Event
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.PostType != "message" {
		t.Errorf("expected 'message', got %q", decoded.PostType)
	}
	if decoded.GroupID != 999 {
		t.Errorf("expected 999, got %d", decoded.GroupID)
	}
	if decoded.Sender.Nickname != "TestUser" {
		t.Errorf("expected 'TestUser', got %q", decoded.Sender.Nickname)
	}
}

func TestEventMessagePrivate(t *testing.T) {
	evt := Event{
		PostType:    "message",
		MessageType: "private",
		UserID:      123,
		RawMessage:  "hi",
	}

	if evt.PostType != "message" || evt.MessageType != "private" {
		t.Errorf("unexpected event type: %s/%s", evt.PostType, evt.MessageType)
	}
}

func TestActionMarshal(t *testing.T) {
	action := Action{
		Action: "send_private_msg",
		Params: map[string]interface{}{
			"user_id": 123,
			"message": "hello",
		},
		Echo: "1",
	}

	data, err := json.Marshal(action)
	if err != nil {
		t.Fatal(err)
	}

	var decoded Action
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.Action != "send_private_msg" {
		t.Errorf("expected 'send_private_msg', got %q", decoded.Action)
	}
}

func TestActionResponse(t *testing.T) {
	resp := ActionResponse{
		Status:  "ok",
		RetCode: 0,
		Data: map[string]interface{}{
			"message_id": 12345,
		},
		Echo: "1",
	}

	data, _ := json.Marshal(resp)
	var decoded ActionResponse
	json.Unmarshal(data, &decoded)

	if decoded.Status != "ok" {
		t.Errorf("expected 'ok', got %q", decoded.Status)
	}
}

func TestStatus(t *testing.T) {
	s := Status{
		AppInitialized: true,
		AppEnabled:     true,
		Online:         true,
		Good:           true,
	}

	if !s.Online || !s.Good {
		t.Error("status should be online and good")
	}
}

func TestEventMetaHeartbeat(t *testing.T) {
	evt := Event{
		PostType:      "meta_event",
		MetaEventType: "heartbeat",
		Interval:      3000,
		Status: &Status{
			Online: true,
			Good:   true,
		},
	}

	if evt.PostType != "meta_event" {
		t.Errorf("expected 'meta_event', got %q", evt.PostType)
	}
	if evt.MetaEventType != "heartbeat" {
		t.Errorf("expected 'heartbeat', got %q", evt.MetaEventType)
	}
	if !evt.Status.Online {
		t.Error("should be online")
	}
}
