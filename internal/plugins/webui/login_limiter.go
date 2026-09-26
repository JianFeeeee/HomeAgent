package webui

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ===== 登录入口的滥用防护 =====
//
// 为什么需要：webui 门户可以经 frp 之类的穿透暴露到公网
// （本项目实测 https://homeagent.jianfgit.xyz/ 直达），而 handleLogin
// 在此之前是**零防护**：无速率限制、无失败计数、口令用 == 明文比对、
// 失败无审计。这等于把一个可爆破的口子直接开到外网。
//
// 设计取舍（都不是「越多越好」）：
//
//   - **按来源 IP 计数**，不全局：全局计数会让攻击者用一个 IP 的狂刷
//     把所有人（含管理员自己）一起锁死 —— 那既是 DoS，又让「锁着」
//     变成一种攻击手段。按来源隔离后，攻击者只能锁自己。
//   - **退避而非永久封禁**：窗口过期自动恢复。永久封禁意味着一旦误撞
//     （或被撞库）就再也登不进，只能上机器改配置。
//   - **成功即清零**：否则「手滑输错三次」会被永久记账。惩罚应只针对
//     持续失败的行为。
//   - **不做账号级锁定**：本系统 webui 只有单一管理员账号，账号级锁定
//     相比 IP 级没有额外收益，却多一个误伤面。
//
// 刻意不做：全局失败告警、验证码、账号锁定 —— 要么超出本系统规模所需，
// 要么会引入新的误伤面。留待真需要时再说。

// loginLimiter 是按来源的失败计数器。
//
// 并发：登录是公网入口，必须扛得住并发爆破，计数用互斥量保护。
// 读路径（Allow）也要加锁 —— 无锁读在多核下会漏计，反而让限流可被
// 多核并发绕过。
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string]*loginFailRecord
	// maxFails 是触发限流的失败次数上限。
	maxFails int
	// window 是失败计数的存活窗口。
	window time.Duration
	// now 可注入，便于测试窗口过期而不必 sleep。
	now func() time.Time
	// offsetInTest 仅测试用：虚拟时钟的累计偏移（生产恒为 0）。
	offsetInTest time.Duration
}

type loginFailRecord struct {
	count int
	first time.Time // 本轮计数的起点（**不是**最后一次失败的时间）
}

func newLoginLimiter(maxFails int, window time.Duration) *loginLimiter {
	return &loginLimiter{
		fails:    map[string]*loginFailRecord{},
		maxFails: maxFails,
		window:   window,
		now:      time.Now,
	}
}

// Allow 报告该来源此刻是否允许再试一次登录。
// 被限流时同时返回建议的重试等待时长。
func (l *loginLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.fails[key]
	if !ok {
		return true, 0
	}
	now := l.now()
	if now.Sub(rec.first) >= l.window {
		// 窗口已过：这条记录不再代表「近期持续失败」，直接丢弃。
		delete(l.fails, key)
		return true, 0
	}
	if rec.count < l.maxFails {
		return true, 0
	}
	// 还能再等多久 —— 用于 Retry-After。下限 1 秒：算出 0 会让客户端
	// 立即重试，那等于没有限流。
	retry := l.window - now.Sub(rec.first)
	if retry < time.Second {
		retry = time.Second
	}
	return false, retry
}

// Fail 记一次失败。**成功登录不调用它**，由 Reset 清零。
func (l *loginLimiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	rec, ok := l.fails[key]
	if !ok || now.Sub(rec.first) >= l.window {
		// 新一轮失败：计数从 1 重新开始，窗口也重新起算。
		l.fails[key] = &loginFailRecord{count: 1, first: now}
		return
	}
	rec.count++
}

// Reset 登录成功后清零该来源的计数。
func (l *loginLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

// prune 丢弃过期记录。
//
// 为什么需要：不调用它就没有别的触发点，map 会随来源无限增长 ——
// 攻击者轮换大量来源 IP 就能把它喂成内存泄漏。
func (l *loginLimiter) prune() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, rec := range l.fails {
		if now.Sub(rec.first) >= l.window {
			delete(l.fails, k)
		}
	}
}

// liveKeys 返回当前记录数（仅测试用；生产路径不读它，避免为测试留后门）。
func (l *loginLimiter) liveKeys() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.fails)
}

// clockAdvance 仅测试用：推进虚拟时钟，使窗口可在不 sleep 的情况下过期。
func (l *loginLimiter) clockAdvance(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.offsetInTest += d
	l.now = func() time.Time { return time.Now().Add(l.offsetInTest) }
}

// sourceKey 取请求的来源标识。
//
// 刻意**不用** X-Forwarded-For：那个头由客户端可伪造，直接采信等于让
// 攻击者随手换一个头就能绕过限流（甚至把限流当成打别人来源的武器）。
// 真实客户端 IP 只能由前置反代决定，那是部署侧的事。
//
// 代价（写明以免误以为它永远精确）：若 webui 直接挂在反代后面，
// 所有请求会共用反代的 IP，限流会退化成「全局」。那种部署应在反代层
// 做限流，或让反代用 PROXY protocol 传真实来源。
func sourceKey(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "unknown"
}

// 限流参数。取「够宽容又不至于被爆破」的值：
//
//	5 次失败后开始拦 —— 记错口令、输错用户名都可能连错几次；持续的
//	  失败才可疑。
//	10 分钟窗口 —— 10 分钟内 5 次错基本可断定是爆破；窗口过长会让一次
//	  撞库把来源锁很久。
//	1MB 请求体上限 —— 真实登录请求只有几十字节，1MB 已有极大余量；
//	  不限制则单个请求就能吃光内存。
const (
	loginMaxFails  = 5
	loginWindow    = 10 * time.Minute
	loginBodyLimit = 1 << 20
)

// enforceLoginRateLimit 是 handleLogin 的限流闸门。
// 放行返回 true；已超限则已写好 429 并返回 false。
func (h *Handler) enforceLoginRateLimit(w http.ResponseWriter, r *http.Request) bool {
	if h.loginLimiter == nil {
		return true
	}
	key := sourceKey(r)
	if ok, retry := h.loginLimiter.Allow(key); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error": "登录尝试过于频繁，请稍后再试",
		})
		return false
	}
	return true
}

// bodyOverLimit 报告请求体是否已超过登录体上限。
//
// 存在理由（重要）：MaxBytesReader 只在**读超限**时通过 MaxBytesError 报错，
// 而 json.Decoder 遇到非法 JSON 会在第 0 字节就返回语法错误，根本不往下读。
// 于是「超大 + 非法」这种最省力的攻击载荷只会被当成 400，而数据已进缓冲。
// 这里显式检查 ContentLength 补上这个缺口。
func bodyOverLimit(r *http.Request) bool {
	return r.ContentLength > loginBodyLimit
}
