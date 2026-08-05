package memory

// 共享测试工具：在 t.TempDir() 中生成小型合成 word2vec 文本模型，
// 替代曾硬编码在 /tmp 的真实 fastText 模型（依赖网络下载与全局文件）。
// 各领域词簇落在正交维度上，语义测试断言即可稳定复现。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// synthClusters 领域词簇：簇索引即向量维度，同簇词共享同一维度。
var synthClusters = map[int][]string{
	0: {"天气", "下雨", "明天", "今天", "台风", "降温", "气象", "预报", "雨"},
	1: {"股票", "基金", "投资", "定投", "收益", "行情", "理财", "风险", "策略", "市场", "stock", "涨"},
	2: {"微积分", "导数", "数学", "作业", "公式", "求解", "计算", "题目"},
	3: {"大学", "招生", "录取", "分数", "医学", "医药", "专业", "分数线", "志愿", "高考", "升学", "排名", "咨询", "河南", "university"},
	4: {"老大", "私聊", "消息", "回复", "汇报", "任务", "安排", "收到", "boss"},
	5: {"图片", "转换", "工具", "图标", "画布", "svg"},
	6: {"南航", "航空", "航天", "电气", "院校", "民航"},
	7: {"前端", "组件", "封装", "布局", "页面", "路由", "交互", "调试", "优化", "代码", "开发", "逻辑", "react", "javascript"},
	8: {"服务器", "配置", "部署", "容器", "代理", "证书", "备份", "恢复", "监控", "告警", "数据库", "反向", "续期", "nginx", "docker", "server", "computer", "电脑"},
}

// synthNeutral 通用词：落在最后一个维度，不参与领域区分。
var synthNeutral = []string{
	"会", "不会", "帮", "查", "看", "最近", "晚上", "随便", "推荐", "电影",
	"注意", "安全", "可以", "说", "事情", "要求", "检查", "状态", "获取",
	"实现", "测试", "结果", "问题", "处理", "已经", "相关", "需要", "使用",
	"方法", "信息", "好的", "内容", "发送", "询问", "朋友",
}

// writeSynthModel 生成合成 word2vec 文本模型文件并返回路径。
func writeSynthModel(t testing.TB, dim int) string {
	t.Helper()

	words := make(map[string][]float64)
	var clusterDims []int
	for c := range synthClusters {
		clusterDims = append(clusterDims, c)
	}
	sort.Ints(clusterDims)

	for _, c := range clusterDims {
		for _, w := range synthClusters[c] {
			vec := make([]float64, dim)
			vec[c] = 1.0
			words[w] = vec
		}
	}
	neutralDim := len(synthClusters)
	for _, w := range synthNeutral {
		vec := make([]float64, dim)
		vec[neutralDim] = 1.0
		words[w] = vec
	}

	path := filepath.Join(t.TempDir(), "synth.vec")
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d %d\n", len(words), dim))
	for w, vec := range words {
		sb.WriteString(w)
		for _, v := range vec {
			sb.WriteString(" ")
			sb.WriteString(strconv.FormatFloat(v, 'f', 4, 64))
		}
		sb.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("writeSynthModel: %v", err)
	}
	return path
}

// newSynthEmbedder 返回加载了合成模型的 StaticEmbedder。
func newSynthEmbedder(t testing.TB, dim int) *StaticEmbedder {
	t.Helper()
	e := NewStaticEmbedder(writeSynthModel(t, dim))
	if !e.Loaded() {
		t.Fatal("synthetic embedder should be loaded")
	}
	return e
}
