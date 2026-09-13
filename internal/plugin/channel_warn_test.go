package plugin

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// 延迟判定的语义：看的是"插件 Start 结束后最终声明了什么"，
// 而不是"注册出站通道的那一刻有没有入站声明"。
//
// 为什么必须这样判：声明顺序自由 —— qq/weather 都是**先** RegisterOutputChannel
// **后** RegisterInputChannel，按注册时刻判会把它们误报成"只声明了输出通道"
// （实测发生过：用户据此以为 qq 插件没更新）。
func TestWarnOutputOnlyChannels(t *testing.T) {
	r := NewRegistry()
	var buf bytes.Buffer
	oldOut := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(oldOut)

	// ① 出站+入站都声明了（先出站后入站）⇒ 不该告警
	r.noteChannel("qq", "qq", true)
	r.noteChannel("qq", "qq", false)
	r.warnOutputOnlyChannels("qq")
	if s := buf.String(); s != "" {
		t.Fatalf("qq 声明了入站通道，不应告警，实际: %s", s)
	}

	// ② 只声明出站 ⇒ 应告警，且只报这一个通道
	buf.Reset()
	r.noteChannel("weather", "weather_weather_out", true)
	r.noteChannel("weather", "weather_weather_in", false)
	r.warnOutputOnlyChannels("weather")
	out := buf.String()
	if !strings.Contains(out, "weather_weather_out") {
		t.Fatalf("只声明出站的通道应被告警，实际: %q", out)
	}
	if strings.Contains(out, "weather_weather_in") {
		t.Fatalf("已声明入站的通道不该被牵连，实际: %q", out)
	}
}
