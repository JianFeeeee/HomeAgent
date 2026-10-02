// Package distill implements triple extraction from raw conversation records
// using a small generation model.
//
// 设计依据（2026-10-02 实测，60 条 order-gw 真实运维记录）
//
// 为什么不用现有的 nlp.Extractor：实测 extractKeyTriples 产出 0 条
// （defaultParser 为 nil，POS 模板抽不出），库里 188 个实体全部由模型主动调
// memory_commit 写入整句复合值。后果是跨维度检索全失效 ——
// 「第181批的值班手册是第几版」这类问题基线 0/5。
//
// 为什么必须用 JSON Schema 约束（这是小模型能用的唯一机制）：
//
//	format: schema 对象   6/6 记录零幻觉，30 对字段
//	format: "json" 字符串  只输出单个对象就 stop（0 对）
//	零样本文本提示         聊天腔「根据您提供的信息…」（0 对）
//	few-shot 续写          结构不匹配时复读示例答案（100% 幻觉）
//
// 三道不可省的闸门：
//  1. 值必须原样出现在原文（唯一的幻觉闸门 —— 生成侧没有向量可验证）
//  2. 维度名归一（不归一就建不了边：实测「停机」/「停机时间」并存）
//  3. 主语锚定（拆开后主语与值变成独立实体，答案会丢 —— 规则拆分实测 0/20）
package distill

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/generation"
)

// fieldsSchema 约束解码目标。实测这是 1.7b 零幻觉的唯一形态。
const fieldsSchema = `{
  "type": "object",
  "properties": {
    "fields": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "name": {"type": "string"},
          "value": {"type": "string"}
        },
        "required": ["name", "value"]
      }
    }
  },
  "required": ["fields"]
}`

// promptTemplate 零样本 + 单示例。示例的作用是给出**输出形状**，
// 不是 few-shot 续写 —— 续写在结构不匹配时会复读示例答案。
//
// 关键：示例只用一条，且要求「value 原样照抄」。
const promptTemplate = `把下面的记录拆成字段，输出 JSON。

规则：
1. fields 每项是 {"name": 字段名, "value": 数值}
2. value 必须从记录里**原样照抄**，不许改写、换算、补全
3. 同一个含义在不同记录里必须用**完全一样**的字段名
4. 记录开头的批次号是主语，不要单独列成字段
5. 没有可拆的字段就输出 {"fields":[]}

记录：第183批 告警规则9条·值班手册第4版·容量预警70%·排期10月
输出：{"fields":[{"name":"告警规则数","value":"9条"},{"name":"值班手册版本","value":"第4版"},{"name":"容量预警","value":"70%"},{"name":"排期","value":"10月"}]}

记录：__RECORD__
输出：`

type Field struct {
	Name  string
	Value string
}

// Extractor 把一条记录拆成 (主语, 维度, 值) 三元组。
type Extractor struct {
	gen generation.Provider
	// subjectFrom 由记录文本推导主语（通常是批次号）。返回空则不产出三元组，
	// 只把字段当作无主语的属性。
	subjectFrom func(record string) string
}

// NewExtractor 构造一个拆分器。subjectFrom 为 nil 时默认按「批次号」推导。
func NewExtractor(gen generation.Provider, subjectFrom func(string) string) *Extractor {
	if subjectFrom == nil {
		subjectFrom = defaultSubject
	}
	return &Extractor{gen: gen, subjectFrom: subjectFrom}
}

// defaultSubject 从记录里取批次号作主语（如「第183批」）。
//
// 为什么必须有主语：规则拆分实测 0/20 —— 拆开后「第183批」和
// 「值班手册第4版」是两个互不相关的实体，图中无边可循，答案就丢了。
// 主语是把这个绑定显式建成边的锚。
func defaultSubject(record string) string {
	// 取最靠前的「第N批」或「第N/M批」形态。
	//
	// 按 rune 迭代：中文是 3 字节，按 byte 比较会因溢出得到错误的结论。
	runes := []rune(record)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '第' {
			continue
		}
		j := i + 1
		for j < len(runes) && (isDigit(runes[j]) || runes[j] == '~' ||
			runes[j] == '/' || runes[j] == '、') {
			j++
		}
		if j < len(runes) && runes[j] == '批' {
			return string(runes[i : j+1])
		}
	}
	return ""
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// Split 把一条记录拆成三元组。
//
// 返回的 triples 已过三道闸门：值原样校验、维度名归一、字段名长度限制。
// 全部字段被闸门拦下时返回空切片而非错误 —— 「这条记录没有可拆的字段」
// 是正常结果（例如「第129~143批均仅评审通过」这类纯叙述句），
// 与「模型没跑起来」是两回事，调用方必须能区分。
func (e *Extractor) Split(ctx context.Context, record string) ([]memory.Triple, error) {
	if strings.TrimSpace(record) == "" {
		return nil, nil
	}
	if e.gen == nil {
		return nil, fmt.Errorf("distill: no generation provider")
	}
	if !e.gen.Info().SupportsJSONSchema {
		// 静默忽略 schema 会让调用方拿到自由文本去 parse JSON ——
		// 那种失败查不出是哪一层的错。必须显式报。
		return nil, fmt.Errorf("distill: provider %q cannot honor json schema: %w",
			e.gen.Info().Model, generation.ErrSchemaUnsupported)
	}

	resp, err := e.gen.Generate(ctx, generation.Request{
		Prompt:      strings.Replace(promptTemplate, "__RECORD__", record, 1),
		MaxTokens:   256,
		Temperature: 0,
		JSONSchema:  fieldsSchema,
	})
	if err != nil {
		return nil, fmt.Errorf("distill: generate: %w", err)
	}

	fields, err := parseFields(resp.Text)
	if err != nil {
		if resp.Truncated {
			// 截断的 JSON 不可救，也不该救 —— 重试比丢弃更划算（模型是随机的）。
			return nil, fmt.Errorf("distill: output truncated at max tokens: %w", err)
		}
		return nil, fmt.Errorf("distill: parse fields: %w", err)
	}

	subject := e.subjectFrom(record)
	var triples []memory.Triple
	for _, f := range fields {
		name := NormalizeDimension(f.Name)
		if name == "" {
			continue
		}
		// 闸门 1：值必须原样来自原文。
		if f.Value == "" || !strings.Contains(record, f.Value) {
			continue
		}
		// 闸门 3：字段名上限。超长的多半是把整句抄了进来当字段名。
		if len([]rune(name)) > 12 {
			continue
		}
		// 有主语时建成三元组（可检索）；无主语时只留字段名作为关系目标，
		// 但那种情况下写不出 subject，直接跳过 —— 宁可少记也不写脏数据。
		if subject == "" {
			continue
		}
		triples = append(triples, memory.Triple{
			Subject:  subject,
			Relation: name,
			Object:   f.Value,
			// 记原句，便于日后从图谱回到原文。
			SentenceText: record,
		})
	}
	return triples, nil
}

func parseFields(text string) ([]Field, error) {
	var envelope struct {
		Fields []Field `json:"fields"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}
	return envelope.Fields, nil
}

// dimensionAliases 把实测出现的同义异名归一到受控词表。
//
// 不归一的后果是建不了边：实测同一含义出现「停机」/「停机时间」两种命名，
// 跨记录查询时两种都要查，且边连不上。
//
// 加词时的判据：这个说法在真实语料里出现过，且确实是同一含义。
// 归一失败保持原名（不丢弃）—— 丢弃会丢信息。
var dimensionAliases = map[string]string{
	"停机时间":   "停机时长",
	"停机时长":   "停机时长",
	"停机":     "停机时长",
	"回滚版本":   "回滚版本",
	"回滚":     "回滚版本",
	"灰度比例":   "灰度比例",
	"灰度":     "灰度比例",
	"发布窗口":   "发布窗口",
	"发布时间":   "发布窗口",
	"时间":     "发布窗口",
	"值班手册":   "值班手册版本",
	"值班手册版本": "值班手册版本",
	"值班手册版":  "值班手册版本",
	"告警规则":   "告警规则数",
	"告警规则数":  "告警规则数",
	"容量预警":   "容量预警",
	"容量":     "容量预警",
	"排期":     "排期",
	"批次号":    "批次号",
	"批号":     "批次号",
	"端口":     "端口",
	"验证状态":   "验证状态",
	"评审状态":   "评审状态",
	"版本数":    "版本数",
	"日期":     "日期",
}

// NormalizeDimension 把字段名归一到受控词表。未命中时返回裁剪后的原名。
func NormalizeDimension(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Trim(name, "*·•-—:：,， ")
	if name == "" {
		return ""
	}
	if canonical, ok := dimensionAliases[name]; ok {
		return canonical
	}
	// 包含式匹配：模型有时输出「停机时长(分钟)」这类带后缀的写法。
	//
	// 挑**最长**匹配别名而不是最短：「停机时长」比「停机」长但更具体，
	// 先命中它才不会退化成更泛的「停机时长」（虽然此处二者同 canonical，
	// 但「排期」vs「排期月」这类一对多映射下，最长匹配才是对的规则）。
	bestAlias := ""
	for alias := range dimensionAliases {
		if strings.Contains(name, alias) && len(alias) > len(bestAlias) {
			bestAlias = alias
		}
	}
	if bestAlias != "" {
		return dimensionAliases[bestAlias]
	}
	return name
}
