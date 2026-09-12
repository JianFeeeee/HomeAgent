package memory

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// 内嵌词库必须自足：把 GOMODCACHE/GOPATH/HOME 全部指向不存在的路径，
// 分词与关键词提取仍然要工作。
//
// 这正是修复前的故障场景：原实现只去猜 Go 模块缓存，部署机上没有那份缓存时
// GetJieba() 返回 nil，四个函数静默返回空列表——关键词提取、文档 TF-IDF 分词、
// NLP 依存解析（进而 doc→graph 三元组抽取）一起失效，且只有一行日志。
func TestEmbeddedJiebaDictIsSelfContained(t *testing.T) {
	t.Setenv("GOMODCACHE", filepath.Join(t.TempDir(), "nonexistent"))
	t.Setenv("GOPATH", filepath.Join(t.TempDir(), "nonexistent"))
	t.Setenv("HOME", filepath.Join(t.TempDir(), "nonexistent"))
	// 清掉可能已被其它测试初始化过的单例。
	jiebaOnce = sync.Once{}
	jiebaInst = nil
	t.Cleanup(func() {
		jiebaOnce = sync.Once{}
		jiebaInst = nil
	})

	if x := GetJieba(); x == nil {
		t.Fatal("模块缓存不可见时 jieba 必须仍能初始化（词库应来自内嵌副本）")
	}

	words := TokenizeWords("今天天气很好，我们去公园散步")
	if len(words) == 0 {
		t.Fatal("分词结果为空：内嵌词库没有真正生效")
	}
	t.Logf("分词结果: %v", words)

	// 内容词（名词/动词/形容词）——依赖词典里的词性列，顺便验证 Tag 路径可用。
	content := TokenizeContentWords("北京是中国的首都，这里有很多历史建筑")
	if len(content) == 0 {
		t.Fatal("内容词为空：Tag（词性标注）路径失效")
	}
	t.Logf("内容词: %v", content)

	if kw := ExtractKeywords("机器学习模型训练需要大量数据和算力"); len(kw) == 0 {
		t.Fatal("关键词为空")
	}
}

// 落盘目录必须幂等：第二次调用不应重写文件（正常启动只做几次 stat）。
func TestMaterializeJiebaDictIsIdempotent(t *testing.T) {
	dir, err := materializeJiebaDict()
	if err != nil {
		t.Fatalf("materializeJiebaDict: %v", err)
	}
	for _, name := range jiebaDictFiles {
		p := filepath.Join(dir, name)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("缺少词库文件 %s: %v", name, err)
		}
		if fi.Size() == 0 {
			t.Fatalf("词库文件 %s 为空", name)
		}
	}

	// 记下 mtime，再调一次，必须完全没动过。
	before, _ := os.Stat(filepath.Join(dir, "jieba.dict.utf8"))
	dir2, err := materializeJiebaDict()
	if err != nil {
		t.Fatal(err)
	}
	if dir2 != dir {
		t.Fatalf("目录名不稳定：%s vs %s（会导致每次启动重建词库）", dir, dir2)
	}
	after, _ := os.Stat(filepath.Join(dir, "jieba.dict.utf8"))
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("已存在完整词库时不应重写文件")
	}
}

// 词库内容哈希必须稳定：否则目录名每次都变，等于每次启动都重建。
func TestJiebaDictDigestStable(t *testing.T) {
	a, err := jiebaDictDigest()
	if err != nil {
		t.Fatal(err)
	}
	b, err := jiebaDictDigest()
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("哈希不稳定: %s vs %s", a, b)
	}
	if len(a) != 16 {
		t.Fatalf("哈希长度异常: %q", a)
	}
}
