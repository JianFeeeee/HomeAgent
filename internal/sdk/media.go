package sdk

import (
	"errors"

	"github.com/JianFeeeee/HomeAgent/internal/memory/media"
)

// ErrMediaUnavailable 表示媒体存储未接线（宿主没注入 media.Store）。
// 单独成一个哨兵：调用方需要区分"没配"与"读失败"，前者不该重试。
var ErrMediaUnavailable = errors.New("media: 媒体存储不可用")

// MediaAPI 是内置插件使用的媒体存储接口（内核侧扩展，非公开 SDK 契约）。
//
// 存在的理由：多模态链路的一切都以**内容寻址**为锚——media.Store 按
// sha256 去重落盘，向量按 digest 存在同一条记录上，知识/文档/记忆块都只
// 存「引用」不存字节。而 WebUI 的上传（handleChatFile）历史上只把文件
// 落在 uploads/ 目录、直接读字节拼 data URL，**从不入 CAS**，于是拿不到
// digest，也就无法把图挂到知识条目上。
//
// 为什么是 internal/sdk 而不是公开 SDK：同上 KnowledgeAPI 多模态方法的
// 理由——third_party/homeagent-sdk/sdk/ 受接口冻结约束，diff 必须恒为 0。
type MediaAPI interface {
	// Put 把字节存入 CAS，返回内容 digest。同一内容重复 Put 幂等
	// （不重复落盘，只刷新 last_seen）。
	Put(data []byte, mime, tool string) (string, error)
	// Stat 读元数据，不读内容。
	Stat(digest string) (MediaInfo, error)
	// Get 读回内容并校验 digest。
	Get(digest string) ([]byte, error)
}

// MediaInfo 是媒体元信息的只读视图。
//
// 刻意与 media.Item 分离（而非直接别名）：那会把 origin_path / first_seen
// 等溯源字段一并暴露给插件层，而插件只需要"这条媒体是什么、怎么取回"。
type MediaInfo struct {
	Digest string `json:"digest"`
	Kind   string `json:"kind"`
	MIME   string `json:"mime"`
	Size   int64  `json:"size"`
}

type mediaImpl struct{ ms *media.Store }

// NewMedia 构造媒体存储接口。ms 为 nil 时各方法安全降级（返回 error / 空值）。
func NewMedia(ms *media.Store) MediaAPI { return &mediaImpl{ms: ms} }

func (m *mediaImpl) Put(data []byte, mime, tool string) (string, error) {
	if m.ms == nil {
		return "", nil
	}
	return m.ms.Put(data, media.Item{MIME: mime, Tool: tool})
}

func (m *mediaImpl) Stat(digest string) (MediaInfo, error) {
	if m.ms == nil {
		return MediaInfo{}, ErrMediaUnavailable
	}
	it, err := m.ms.Stat(digest)
	if err != nil {
		return MediaInfo{}, err
	}
	return MediaInfo{
		Digest: it.Digest,
		Kind:   string(it.Kind),
		MIME:   it.MIME,
		Size:   it.Size,
	}, nil
}

func (m *mediaImpl) Get(digest string) ([]byte, error) {
	if m.ms == nil {
		return nil, ErrMediaUnavailable
	}
	return m.ms.Get(digest)
}

var _ MediaAPI = (*mediaImpl)(nil)
